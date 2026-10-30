package orgs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/applink"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/page"
	"koro/internal/platform/queue"
)

// The review of a pending_review org (BE-144; Q-33: company registration
// is open to everyone and a platform admin verifies it). createCompany
// writes org.review_requested, adminVerifyOrg and the rejection (closeOrg
// of a pending_review org by platform staff) org.review_decided, each in
// its business transaction (O: the people waiting for the decision must
// hear of it). The workers below turn them into notices.

// Worker names (01 §6.2).
const (
	// NotifyReviewRequestedTaskName consumes org.review_requested: the
	// platform admins hear of a new org to verify (in-app).
	NotifyReviewRequestedTaskName = "orgs.notify_review_requested"
	// NotifyReviewDecidedTaskName consumes org.review_decided: the org's
	// admins hear of the verification, the founder of the rejection and
	// its reason (in-app and e-mail).
	NotifyReviewDecidedTaskName = "orgs.notify_review_decided"
)

// Notice templates (internal/notify/templates/orgs).
const (
	tmplReviewRequested = "orgs.review_requested" // platform admins: in-app
	tmplOrgVerified     = "orgs.org_verified"     // the org's admins: in-app + e-mail
	tmplOrgRejected     = "orgs.org_rejected"     // the founder: in-app + e-mail with the reason
)

// emitReviewRequested writes org.review_requested (O) in the caller's
// transaction.
func (s *service) emitReviewRequested(ctx context.Context, o Org) error {
	return outbox.Emit(ctx, s.d.Outbox, domainevent.OrgReviewRequested{OrgID: o.ID, OrgKind: string(o.Kind)}, orgAggType, o.ID)
}

// emitReviewDecided writes org.review_decided (O) in the caller's
// transaction; outcome is domainevent.OrgReviewVerified or
// OrgReviewRejected.
func (s *service) emitReviewDecided(ctx context.Context, o Org, outcome string) error {
	return outbox.Emit(ctx, s.d.Outbox, domainevent.OrgReviewDecided{OrgID: o.ID, OrgKind: string(o.Kind), Outcome: outcome},
		orgAggType, o.ID)
}

// rejection reports whether a closes o as the platform's rejection of its
// registration: o awaits review and a is not its company_owner (an owner
// closing their own pending company withdraws it: nobody is told).
func rejection(a authz.Actor, o Org) bool {
	return o.Status == StatusPendingReview && !a.HasOrgRole(o.ID, authz.MemberCompanyOwner)
}

// adminRoles are the roles told of an org's verification: the ones that
// run its panel.
func adminRoles(k Kind) []authz.MemberRole {
	switch k {
	case KindCompany:
		return []authz.MemberRole{authz.MemberCompanyOwner, authz.MemberCompanyAdmin}
	case KindClub:
		return []authz.MemberRole{authz.MemberClubAdmin}
	case KindUniversity:
		return []authz.MemberRole{authz.MemberUniversityRep}
	}
	return nil
}

// handleReviewRequested is orgs.notify_review_requested: one in-app notice
// per platform admin, linking the admin list of orgs awaiting review. An
// org decided meanwhile needs no review. Idempotent: the dedupe key holds
// the event and the admin.
func (s *service) handleReviewRequested(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OrgReviewRequested](e)
	if err != nil {
		return queue.Permanent(err)
	}
	o, ok, err := s.reviewedOrg(ctx, p.OrgID)
	if err != nil || !ok || o.Status != StatusPendingReview {
		return err
	}
	if s.d.Sender == nil || s.d.PlatformAdmins == nil {
		return errors.New("orgs: the review notices are not wired")
	}
	admins, err := s.d.PlatformAdmins(ctx)
	if err != nil {
		return fmt.Errorf("orgs: platform admins: %w", err)
	}
	return s.notifyEach(ctx, e, o, tmplReviewRequested, admins, notify.AdminPendingOrgsLink(), nil)
}

// handleReviewDecided is orgs.notify_review_decided. A verification tells
// the org's active admins (adminRoles) in-app, linking the org's panel,
// and by e-mail; a rejection tells the founder in-app and by e-mail, which
// carries the reason (organizations.rejection_reason; the in-app text is
// fixed). Idempotent: the dedupe keys hold the event, the user and the
// channel.
func (s *service) handleReviewDecided(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OrgReviewDecided](e)
	if err != nil {
		return queue.Permanent(err)
	}
	o, ok, err := s.reviewedOrg(ctx, p.OrgID)
	if err != nil || !ok {
		return err
	}
	if s.d.Sender == nil {
		return errors.New("orgs: the review notices are not wired")
	}
	switch p.Outcome {
	case domainevent.OrgReviewVerified:
		if o.VerifiedAt == nil {
			return nil
		}
		to, err := s.activeAdmins(ctx, o)
		if err != nil {
			return err
		}
		link := notify.OrgLink(o.ID)
		return s.notifyEach(ctx, e, o, tmplOrgVerified, to, link, map[string]any{
			"org_name": o.Name, "link": applink.Page(s.d.LinkBaseURL, link.String()),
		})
	case domainevent.OrgReviewRejected:
		if o.Status != StatusClosed || o.CreatedBy == nil {
			return nil
		}
		reason := ""
		if o.RejectionReason != nil {
			reason = *o.RejectionReason
		}
		// No link: the founder's membership ended with the org.
		return s.notifyEach(ctx, e, o, tmplOrgRejected, []uuid.UUID{*o.CreatedBy}, notify.Link{}, map[string]any{
			"org_name": o.Name, "reason": reason,
		})
	}
	return queue.Permanent(fmt.Errorf("orgs: org.review_decided: unknown outcome %q", p.Outcome))
}

// reviewedOrg reads the org of a review event; a missing one is logged and
// skipped (orgs are never deleted: a foreign or test ID).
func (s *service) reviewedOrg(ctx context.Context, id uuid.UUID) (Org, bool, error) {
	o, err := s.orgs().GetOrg(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "review notice: org not found", slog.String("org_id", id.String()))
		return Org{}, false, nil
	case err != nil:
		return Org{}, false, err
	}
	return o, true, nil
}

// activeAdmins returns the users holding one of adminRoles(o.Kind) in o,
// active, each once.
func (s *service) activeAdmins(ctx context.Context, o Org) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, role := range adminRoles(o.Kind) {
		var after *MemberAfter
		for {
			rows, err := s.members().ListMembers(ctx, o.ID, MemberFilter{Statuses: []MemberStatus{MemberActive}, Role: role},
				after, page.MaxLimit)
			if err != nil {
				return nil, fmt.Errorf("orgs: admins of %s: %w", o.ID, err)
			}
			for _, m := range rows {
				if !slices.Contains(out, m.UserID) {
					out = append(out, m.UserID)
				}
			}
			if len(rows) < page.MaxLimit {
				break
			}
			after = &MemberAfter{ID: rows[len(rows)-1].ID}
		}
	}
	return out, nil
}

// notifyEach sends tmpl about o to every user in to: in-app (the org's ID
// only, linking link) and, when mail is not nil, by e-mail to the user's
// address with mail plus the user's first name ("name"), in the user's
// locale. A user without a live account gets the in-app notice only (notify
// drops it for an erased one). A permanent send failure is logged and
// skipped; any other is returned (the task retries; the dedupe keys keep
// the delivered ones from repeating).
func (s *service) notifyEach(ctx context.Context, e domainevent.Envelope, o Org, tmpl string, to []uuid.UUID, link notify.Link,
	mail map[string]any) error {
	org := o.ID
	key := tmpl + ":" + e.ID.String() + ":"
	var errs []error
	for _, uid := range to {
		u := uid
		_, err := s.d.Sender.Send(ctx, notify.Message{
			UserID: &u, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
			SourceOrgID: &org, LinkPath: link, Data: map[string]any{"org_id": org.String()},
			DedupeKey: key + u.String() + ":in_app",
		})
		errs = append(errs, s.sendErr(ctx, tmpl, o.ID, u, err))
		if mail == nil {
			continue
		}
		c, ok, err := s.contact(ctx, u)
		if err != nil || !ok {
			errs = append(errs, err)
			continue
		}
		data := map[string]any{"name": c.FirstName}
		for k, v := range mail {
			data[k] = v
		}
		_, err = s.d.Sender.Send(ctx, notify.Message{
			UserID: &u, ToEmail: c.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
			Template: tmpl, Locale: c.Locale, SourceOrgID: &org, Data: data, DedupeKey: key + u.String() + ":email",
		})
		errs = append(errs, s.sendErr(ctx, tmpl, o.ID, u, err))
	}
	return errors.Join(errs...)
}

// contact returns the address of a live user; an unknown or erased user,
// or no Users port, gets no e-mail.
func (s *service) contact(ctx context.Context, id uuid.UUID) (identity.UserContact, bool, error) {
	if s.d.Users == nil {
		return identity.UserContact{}, false, nil
	}
	c, err := s.d.Users.Contact(ctx, id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return identity.UserContact{}, false, nil
	case err != nil:
		return identity.UserContact{}, false, fmt.Errorf("orgs: contact of %s: %w", id, err)
	}
	return c, true, nil
}

// sendErr keeps a retryable send error and logs a permanent one (the notice
// cannot be delivered; retrying changes nothing).
func (s *service) sendErr(ctx context.Context, tmpl string, orgID, userID uuid.UUID, err error) error {
	if err == nil || !notify.IsPermanent(err) {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelError, "review notice not deliverable", slog.String("template", tmpl),
		slog.String("org_id", orgID.String()), slog.String("user_id", userID.String()), slog.String("err", err.Error()))
	return nil
}
