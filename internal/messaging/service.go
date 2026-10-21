package messaging

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/feed"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// ---- the company's side (04 §2.9)

// Create implements Service. Order (03 §3.2, k19; 01 §6.3 "İletişim
// talebi"): authz.Can on the company's surface → the company (403
// orgs.org_suspended / orgs.org_not_verified) → the body (422) → the
// opportunity call (422) → in one transaction with the Idempotency-Key:
// the pseudonym's student, discoverable now, not blocking the company, not
// declined within 90 days (404 for all of them) → no pending request (409
// messaging.request_pending; one past its 14 days is expired on the spot)
// → one contact_request unit (403 entitlements.not_entitled) → the row →
// contact_request.created (O) → the audit record.
func (s *service) Create(ctx context.Context, a authz.Actor, orgID uuid.UUID, in CreateInput) (CompanyView, error) {
	if err := authz.Can(ctx, a, authz.MessagingContactRequestCreate, companySurface(orgID)); err != nil {
		return CompanyView{}, err
	}
	if _, err := s.company(ctx, orgID, true, true); err != nil {
		return CompanyView{}, err
	}
	in, err := cleanCreate(in)
	if err != nil {
		return CompanyView{}, err
	}
	via := ViaDiscovery
	if in.OpportunityPostID != nil {
		if err := s.checkOpportunity(ctx, orgID, *in.OpportunityPostID); err != nil {
			return CompanyView{}, err
		}
		via = ViaOpportunityPost
	}
	hash, err := idempotency.Hash(struct {
		Op  string
		Org uuid.UUID
		In  CreateInput
	}{"create_contact_request", orgID, in})
	if err != nil {
		return CompanyView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		return s.create(ctx, a, orgID, via, in)
	})
	if err != nil {
		return CompanyView{}, err
	}
	r, err := s.d.Store.Get(ctx, orgID, id)
	if err != nil {
		return CompanyView{}, err
	}
	return s.companyView(r)
}

// create is Create's transaction (k19, the unit, the row, the event).
func (s *service) create(ctx context.Context, a authz.Actor, orgID uuid.UUID, via Via, in CreateInput) (uuid.UUID, error) {
	student, err := s.target(ctx, orgID, in.AnonID)
	if err != nil {
		return uuid.Nil(), err
	}
	now := s.now()
	switch cur, err := s.d.Store.Pending(ctx, orgID, student); {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return uuid.Nil(), err
	case cur.Open(now):
		return uuid.Nil(), ErrRequestPending
	default: // past its 14 days: the job has not closed it yet
		if _, err := s.expire(ctx, cur); err != nil {
			return uuid.Nil(), err
		}
	}
	id := uuid.NewV7()
	usage, err := s.d.Entitlements.Consume(ctx, orgID, entitlements.FeatureContactRequest, 1,
		entitlements.Ref{Type: entitlements.RefContactRequest, ID: id})
	if err != nil {
		return uuid.Nil(), err
	}
	r, err := s.d.Store.Insert(ctx, ContactRequest{
		ID: id, OrgID: orgID, SenderUserID: a.UserID, StudentUserID: student, Via: via,
		OpportunityPostID: in.OpportunityPostID, Subject: in.Subject, Message: in.Message, Status: StatusPending,
		ExpiresAt: now.Add(RequestTTL), EntitlementUsageID: usage,
	})
	if err != nil {
		return uuid.Nil(), err
	}
	// The event carries IDs only: the subject and the message never leave
	// the request (01 §6.2: "mesaj gövdesi bildirimde yok").
	if err := s.emit(ctx, domainevent.ContactRequestCreated{
		ContactRequestID: r.ID, OrgID: r.OrgID, StudentUserID: r.StudentUserID,
	}, r.ID); err != nil {
		return uuid.Nil(), err
	}
	org := orgID
	ev := requestEvent(authz.MessagingContactRequestCreate, r, &org, map[string]any{
		"via": string(via), "anon_id": in.AnonID, "entitlement_usage_id": usage.String(),
	})
	ev.ActorOrgID = &org
	return r.ID, s.d.Audit.Log(ctx, ev.By(a))
}

// target resolves the pseudonym to a student the company may ask (03 k19);
// every refusal is the same 404.
func (s *service) target(ctx context.Context, orgID uuid.UUID, anonID string) (uuid.UUID, error) {
	if s.d.Resolve == nil {
		return uuid.Nil(), ErrNotFound
	}
	student, found, err := s.d.Resolve(ctx, orgID, anonID)
	switch {
	case err != nil:
		return uuid.Nil(), err
	case !found || student == uuid.Nil():
		return uuid.Nil(), ErrNotFound
	}
	// Read again here: the resolver answers for discovery, messaging
	// decides for itself (a minor or a lapsed student never qualifies).
	ok, err := s.d.Students.IsDiscoverable(ctx, student)
	switch {
	case err != nil:
		return uuid.Nil(), err
	case !ok:
		return uuid.Nil(), ErrNotFound
	}
	switch blocked, err := s.d.Store.Blocked(ctx, student, orgID); {
	case err != nil:
		return uuid.Nil(), err
	case blocked:
		return uuid.Nil(), ErrNotFound
	}
	switch declined, err := s.d.Store.DeclinedSince(ctx, orgID, student, s.now().Add(-DeclineCooldown)); {
	case err != nil:
		return uuid.Nil(), err
	case declined:
		return uuid.Nil(), ErrNotFound
	}
	return student, nil
}

// checkOpportunity accepts a published opportunity call of the company
// (feed.Posts.Get); anything else is the same 422 (another org's post is
// not told apart from a missing one).
func (s *service) checkOpportunity(ctx context.Context, orgID, postID uuid.UUID) error {
	bad := fieldErr("body.opportunity_post_id", "must be a published opportunity call of this organization")
	if s.d.Posts == nil {
		return bad
	}
	p, err := s.d.Posts.Get(ctx, postID)
	switch {
	case errors.Is(err, feed.ErrNotFound):
		return bad
	case err != nil:
		return err
	case p.OrgID != orgID || p.Kind != feed.KindOpportunityCall || p.Status != feed.StatusPublished:
		return bad
	}
	return nil
}

// companyView is the company's view of r: the pseudonym of the student and
// the effective status.
func (s *service) companyView(r ContactRequest) (CompanyView, error) {
	anon, err := s.d.Pseudonyms.AnonID(r.OrgID, r.StudentUserID)
	if err != nil {
		return CompanyView{}, err
	}
	return CompanyView{Request: r, AnonID: anon, Status: r.Effective(s.now())}, nil
}

// ListSent implements Service.
func (s *service) ListSent(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ListQuery) (CompanyPage, error) {
	if err := authz.Can(ctx, a, authz.MessagingContactRequestListSent, companySurface(orgID)); err != nil {
		return CompanyPage{}, err
	}
	if _, err := s.company(ctx, orgID, false, false); err != nil {
		return CompanyPage{}, err
	}
	if q.Status != "" && !q.Status.Valid() {
		return CompanyPage{}, fieldErr("query.status", "must be pending, accepted, declined, expired or withdrawn")
	}
	limit := page.ClampLimit(q.Limit)
	q.Now, q.Limit = s.now(), limit+1
	rows, err := s.d.Store.ListByOrg(ctx, orgID, q)
	if err != nil {
		return CompanyPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := CompanyPage{Items: make([]CompanyView, len(rows))}
	for i, r := range rows {
		if out.Items[i], err = s.companyView(r); err != nil {
			return CompanyPage{}, err
		}
	}
	if more {
		last := rows[len(rows)-1].ID
		out.Next = &last
	}
	return out, nil
}

// sent reads a request of the company path: authz on the surface first
// (403 for staff and members without a company role, 404 for outsiders),
// then the row of that company (another org's request is 404), then authz
// on the request itself.
func (s *service) sent(ctx context.Context, a authz.Actor, act authz.Action, orgID, id uuid.UUID) (ContactRequest, error) {
	if err := authz.Can(ctx, a, act, companySurface(orgID)); err != nil {
		return ContactRequest{}, err
	}
	r, err := s.d.Store.Get(ctx, orgID, id)
	if err != nil {
		return ContactRequest{}, err
	}
	if err := authz.Can(ctx, a, act, companyResource(r, r.Effective(s.now()))); err != nil {
		return ContactRequest{}, err
	}
	return r, nil
}

// GetSent implements Service.
func (s *service) GetSent(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (CompanyView, error) {
	r, err := s.sent(ctx, a, authz.MessagingContactRequestRead, orgID, id)
	if err != nil {
		return CompanyView{}, err
	}
	return s.companyView(r)
}

// Withdraw implements Service: a pending request becomes withdrawn (the
// unit stays used, 04 §2.9); withdrawing a withdrawn request changes
// nothing; any other status is 409 messaging.invalid_state. The student is
// not notified (the request shows as withdrawn).
func (s *service) Withdraw(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (CompanyView, error) {
	if _, err := s.sent(ctx, a, authz.MessagingContactRequestWithdraw, orgID, id); err != nil {
		return CompanyView{}, err
	}
	if _, err := s.company(ctx, orgID, false, true); err != nil {
		return CompanyView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op  string
		Org uuid.UUID
		ID  uuid.UUID
	}{"withdraw_contact_request", orgID, id})
	if err != nil {
		return CompanyView{}, err
	}
	if _, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.Lock(ctx, id)
		switch {
		case err != nil:
			return uuid.Nil(), err
		case cur.OrgID != orgID:
			return uuid.Nil(), ErrNotFound
		case cur.Status == StatusWithdrawn:
			return cur.ID, nil
		case !cur.Open(s.now()):
			return uuid.Nil(), ErrInvalidState
		}
		next, ok, err := s.d.Store.Transition(ctx, id, StatusPending, StatusWithdrawn, nil)
		switch {
		case err != nil:
			return uuid.Nil(), err
		case !ok:
			return uuid.Nil(), ErrInvalidState
		}
		org := orgID
		ev := requestEvent(authz.MessagingContactRequestWithdraw, next, &org, map[string]any{"unit_released": false})
		ev.ActorOrgID = &org
		return next.ID, s.d.Audit.Log(ctx, ev.By(a))
	}); err != nil {
		return CompanyView{}, err
	}
	r, err := s.d.Store.Get(ctx, orgID, id)
	if err != nil {
		return CompanyView{}, err
	}
	return s.companyView(r)
}
