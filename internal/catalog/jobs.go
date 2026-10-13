package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Worker task names of catalog (01 §6.2).
const (
	CancelEventOfferingsTaskName    = cancelEventOfferings // event.cancelled → the bound offerings
	NotifyOfferingSubmittedTaskName = "catalog.notify_offering_submitted"
	NotifyOfferingDecidedTaskName   = "catalog.notify_offering_decided"
	NotifyOfferingCancelledTaskName = "catalog.notify_offering_cancelled"
)

// Notice templates (internal/notify/templates/catalog). In-app texts are
// fixed (no data): the deep link carries the offering.
const (
	tmplReviewPending = "catalog.offering_review_pending" // platform admins; the university's reps (T-21)
	tmplApproved      = "catalog.offering_approved"       // the seller's admins: on sale
	tmplRejected      = "catalog.offering_rejected"       // the seller's admins: back to draft
	tmplCancelled     = "catalog.offering_cancelled"      // the seller's admins
	tmplPolicyChanged = "catalog.commerce_policy_changed" // every platform and finance admin (06 H-17)
)

// Subscribe registers the outbox subscriptions of catalog (01 §6.2):
// event.cancelled → cancel the bound offerings; offering.submitted,
// .decided and .cancelled → the notices.
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicEventCancelled, CancelEventOfferingsTaskName, m.svc.handleEventCancelled,
		outbox.Queue(queue.Critical))
	q := outbox.Queue(queue.Default)
	r.Subscribe(domainevent.TopicOfferingSubmitted, NotifyOfferingSubmittedTaskName, m.svc.handleSubmitted, q)
	r.Subscribe(domainevent.TopicOfferingDecided, NotifyOfferingDecidedTaskName, m.svc.handleDecided, q)
	r.Subscribe(domainevent.TopicOfferingCancelled, NotifyOfferingCancelledTaskName, m.svc.handleCancelled, q)
}

// Tasks returns the plain task handlers of catalog for the worker.
func (m Module) Tasks() []queue.Task {
	return []queue.Task{{Name: NotifyPolicyChangedTaskName, Handle: m.svc.handlePolicyChanged}}
}

// handleSubmitted tells the approvers that an offering waits: every
// platform admin and, when its university must approve it, the
// university's reps.
func (s *service) handleSubmitted(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OfferingSubmitted](e)
	if err != nil {
		return queue.Permanent(err)
	}
	o, ok, err := s.noticeOffering(ctx, p.OfferingID)
	if err != nil || !ok {
		return err
	}
	if s.d.Staff == nil {
		return errors.New("catalog: the staff lookup is not wired")
	}
	admins, err := s.d.Staff.PlatformAdmins(ctx)
	if err != nil {
		return fmt.Errorf("catalog: platform admins: %w", err)
	}
	key := tmplReviewPending + ":" + o.ID.String() + ":" + e.ID.String()
	errs := []error{s.notifyInApp(ctx, o, tmplReviewPending, key, admins, "/admin/offerings")}
	if o.RequiresUniversityApproval {
		f, err := s.facts(ctx, o)
		if err != nil {
			return err
		}
		if f.universityOrgID != uuid.Nil() {
			reps, err := s.membersWith(ctx, f.universityOrgID, authz.MemberUniversityRep)
			if err != nil {
				return err
			}
			errs = append(errs, s.notifyInApp(ctx, o, tmplReviewPending, key, reps,
				"/orgs/"+f.universityOrgID.String()+"/offering-approvals"))
		}
	}
	return errors.Join(errs...)
}

// handleDecided tells the seller's admins the outcome that concerns them:
// on sale, or back to draft (a partial approval waits silently).
func (s *service) handleDecided(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OfferingDecided](e)
	if err != nil {
		return queue.Permanent(err)
	}
	var tmpl string
	switch p.Outcome {
	case OutcomeApproved:
		tmpl = tmplApproved
	case OutcomeRejected, OutcomeUniversityRejected:
		tmpl = tmplRejected
	case OutcomeAwaitingUniversity, OutcomeUniversityApproved:
		return nil
	default:
		return queue.Permanent(fmt.Errorf("catalog: decided notice: unknown outcome %q", p.Outcome))
	}
	return s.notifySeller(ctx, p.OfferingID, tmpl, tmpl+":"+p.OfferingID.String()+":"+e.ID.String())
}

// handleCancelled tells the seller's admins that the offering was
// cancelled (orders handles its own consequences, BE-040).
func (s *service) handleCancelled(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OfferingCancelled](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.notifySeller(ctx, p.OfferingID, tmplCancelled, tmplCancelled+":"+p.OfferingID.String())
}

func (s *service) notifySeller(ctx context.Context, offeringID uuid.UUID, tmpl, key string) error {
	o, ok, err := s.noticeOffering(ctx, offeringID)
	if err != nil || !ok {
		return err
	}
	admins, err := s.membersWith(ctx, o.OrgID, authz.MemberClubAdmin, authz.MemberUniversityRep)
	if err != nil {
		return err
	}
	return s.notifyInApp(ctx, o, tmpl, key, admins, "/orgs/"+o.OrgID.String()+"/offerings")
}

// noticeOffering reads the offering of a notice; a missing one (never
// deleted but a draft: a stale task) is skipped.
func (s *service) noticeOffering(ctx context.Context, id uuid.UUID) (Offering, bool, error) {
	o, err := s.d.Store.GetOffering(ctx, id, false)
	switch {
	case errors.Is(err, ErrNotFound):
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "offering notice: offering not found", slog.String("offering_id", id.String()))
		return Offering{}, false, nil
	case err != nil:
		return Offering{}, false, err
	}
	return o, true, nil
}

// membersWith returns the org's active members holding one of roles.
func (s *service) membersWith(ctx context.Context, orgID uuid.UUID, roles ...authz.MemberRole) ([]uuid.UUID, error) {
	ids, err := s.d.Orgs.ActiveMemberIDs(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("catalog: org members: %w", err)
	}
	var out []uuid.UUID
	for _, id := range ids {
		ok, err := s.d.Orgs.HasRole(ctx, orgID, id, roles...)
		if err != nil {
			return nil, fmt.Errorf("catalog: org member role: %w", err)
		}
		if ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// notifyInApp sends an in-app notice (IDs only) to every recipient; the
// dedupe key makes a repeated task a no-op per recipient.
func (s *service) notifyInApp(ctx context.Context, o Offering, tmpl, key string, to []uuid.UUID, link string) error {
	if len(to) == 0 {
		return nil
	}
	if s.d.Sender == nil {
		return errors.New("catalog: notify is not wired")
	}
	org := o.OrgID
	var errs []error
	for _, uid := range to {
		u := uid
		_, err := s.d.Sender.Send(ctx, notify.Message{
			UserID: &u, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
			SourceOrgID: &org, LinkPath: link, DedupeKey: key + ":" + u.String(),
			Data: map[string]any{"offering_id": o.ID.String()},
		})
		switch {
		case err == nil:
		case notify.IsPermanent(err):
			s.d.Log.LogAttrs(ctx, slog.LevelError, "offering notice not deliverable", slog.String("template", tmpl),
				slog.String("offering_id", o.ID.String()), slog.String("user_id", u.String()), slog.String("err", err.Error()))
		default:
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// handlePolicyChanged tells every platform and finance admin in effect
// (06 H-17) that a commerce policy was created or closed: in-app and by
// e-mail with the policy's parameters (configuration, no personal data).
// A lost e-mail is retried, never dropped silently.
func (s *service) handlePolicyChanged(ctx context.Context, payload []byte) error {
	var n policyNotice
	if err := json.Unmarshal(payload, &n); err != nil || n.PolicyID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("catalog: policy notice payload: %w", errors.Join(err, errors.New("policy_id required"))))
	}
	p, err := s.d.Store.GetPolicy(ctx, n.PolicyID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return queue.Permanent(fmt.Errorf("catalog: policy notice: policy %s not found", n.PolicyID))
	case err != nil:
		return err
	}
	if s.d.Staff == nil || s.d.Sender == nil {
		return errors.New("catalog: the policy notice is not wired")
	}
	pads, err := s.d.Staff.PlatformAdmins(ctx)
	if err != nil {
		return fmt.Errorf("catalog: platform admins: %w", err)
	}
	fins, err := s.d.Staff.FinanceAdmins(ctx)
	if err != nil {
		return fmt.Errorf("catalog: finance admins: %w", err)
	}
	to := slices.Compact(slices.SortedFunc(slices.Values(append(pads, fins...)), func(a, b uuid.UUID) int {
		return compareUUID(a, b)
	}))
	data := policyMailData(p, n.Change)
	key := tmplPolicyChanged + ":" + p.ID.String() + ":" + n.Change
	var errs []error
	for _, uid := range to {
		u := uid
		msgs := []notify.Message{{
			UserID: &u, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmplPolicyChanged,
			LinkPath: "/admin/commerce-policies", DedupeKey: key + ":" + u.String() + ":in_app",
			Data: map[string]any{"policy_id": p.ID.String()},
		}}
		c, ok, err := s.contact(ctx, u)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			mail := map[string]any{"name": c.FirstName}
			for k, v := range data {
				mail[k] = v
			}
			msgs = append(msgs, notify.Message{
				UserID: &u, ToEmail: c.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
				Template: tmplPolicyChanged, Locale: c.Locale, Data: mail, DedupeKey: key + ":" + u.String() + ":email",
			})
		}
		for _, m := range msgs {
			_, err := s.d.Sender.Send(ctx, m)
			switch {
			case err == nil:
			case notify.IsPermanent(err):
				s.d.Log.LogAttrs(ctx, slog.LevelError, "policy notice not deliverable", slog.String("policy_id", p.ID.String()),
					slog.String("user_id", u.String()), slog.String("channel", string(m.Channel)), slog.String("err", err.Error()))
			default:
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// policyMailData is what the policy e-mail shows (configuration only).
func policyMailData(p CommercePolicy, change string) map[string]any {
	scope := string(p.Scope)
	if p.ScopeID != nil {
		scope += " " + p.ScopeID.String()
	}
	out := map[string]any{
		"change": change, "scope": scope, "model_code": string(p.ModelCode),
		"commission_rate":  strconv.FormatFloat(float64(p.CommissionRateBP)/100, 'f', -1, 64),
		"commission_payer": string(p.CommissionPayer), "valid_from": p.ValidFrom.UTC().Format(time.RFC3339),
		"valid_to": "",
	}
	if p.ValidTo != nil {
		out["valid_to"] = p.ValidTo.UTC().Format(time.RFC3339)
	}
	return out
}

// contact returns the address of a live user; an unknown or erased user,
// or no Users port, is in-app only.
func (s *service) contact(ctx context.Context, id uuid.UUID) (identity.UserContact, bool, error) {
	if s.d.Users == nil {
		return identity.UserContact{}, false, nil
	}
	c, err := s.d.Users.Contact(ctx, id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return identity.UserContact{}, false, nil
	case err != nil:
		return identity.UserContact{}, false, fmt.Errorf("catalog: notice contact: %w", err)
	}
	return c, c.Email != "", nil
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}
