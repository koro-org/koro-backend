package messaging

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/feed"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// ---- the student's side (04 §2.9)

// ListMine implements Service: the requests addressed to the caller, newest
// first, with the company's card and the opportunity call.
func (s *service) ListMine(ctx context.Context, a authz.Actor, q ListQuery) (StudentPage, error) {
	if err := authz.Can(ctx, a, authz.MessagingContactRequestListReceived, ownSurface(a)); err != nil {
		return StudentPage{}, err
	}
	if q.Status != "" && !q.Status.Valid() {
		return StudentPage{}, problem.Field("query.status", "must be pending, accepted, declined, expired or withdrawn")
	}
	limit := page.ClampLimit(q.Limit)
	q.Now, q.Limit = s.now(), limit+1
	rows, err := s.requests().ListByStudent(ctx, a.UserID, q)
	if err != nil {
		return StudentPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	views, err := s.studentViews(ctx, rows)
	if err != nil {
		return StudentPage{}, err
	}
	out := StudentPage{Items: views}
	if more {
		last := rows[len(rows)-1].ID
		out.Next = &last
	}
	return out, nil
}

// studentViews adds the company cards (orgs) and the opportunity calls
// (feed) to rows.
func (s *service) studentViews(ctx context.Context, rows []ContactRequest) ([]StudentView, error) {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.OrgID)
	}
	names, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]StudentView, len(rows))
	for i, r := range rows {
		v := StudentView{Request: r, Company: names[r.OrgID], Status: r.Effective(now)}
		if r.OpportunityPostID != nil {
			if v.Post, err = s.postRef(ctx, *r.OpportunityPostID); err != nil {
				return nil, err
			}
		}
		out[i] = v
	}
	return out, nil
}

// postRef is the opportunity call of a request; its title only while the
// post is published (a removed post keeps its ID, nothing else).
func (s *service) postRef(ctx context.Context, postID uuid.UUID) (*PostRef, error) {
	ref := &PostRef{ID: postID}
	if s.d.Posts == nil {
		return ref, nil
	}
	p, err := s.d.Posts.Get(ctx, postID)
	switch {
	case errors.Is(err, feed.ErrNotFound):
		return ref, nil
	case err != nil:
		return nil, err
	}
	if p.Status == feed.StatusPublished {
		ref.Title = p.Title
	}
	return ref, nil
}

// mine reads one of the caller's requests: authz on the own surface, the
// row addressed to the caller (another student's request is 404), authz on
// the request.
func (s *service) mine(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) (ContactRequest, error) {
	if err := authz.Can(ctx, a, act, ownSurface(a)); err != nil {
		return ContactRequest{}, err
	}
	r, err := s.requests().GetForStudent(ctx, a.UserID, id)
	if err != nil {
		return ContactRequest{}, err
	}
	if err := authz.Can(ctx, a, act, ownResource(r, r.Effective(s.now()))); err != nil {
		return ContactRequest{}, err
	}
	return r, nil
}

// GetMine implements Service.
func (s *service) GetMine(ctx context.Context, a authz.Actor, id uuid.UUID) (StudentView, error) {
	r, err := s.mine(ctx, a, authz.MessagingContactRequestRead, id)
	if err != nil {
		return StudentView{}, err
	}
	views, err := s.studentViews(ctx, []ContactRequest{r})
	if err != nil {
		return StudentView{}, err
	}
	return views[0], nil
}

// Respond implements Service. Order: authz (O) → the decision (422) → an
// acceptance needs an active student verification (403
// students.verification_required; a decline never does) → share_contact
// only with an acceptance and with the contact_info_share text shown (422,
// nothing written) → in one transaction: the row lock, the same answer
// again changes nothing, a request that is no longer pending (or past its
// 14 days) is 409 messaging.invalid_state → the status → on acceptance
// students.AccessGrants.Grant (level 3) and, with share_contact,
// students.ContactSharing.ShareContactWithCompany (the e-mail to this
// company alone; both the same transaction: 01 §6.3) and the conversation
// of the acceptance (BE-162) → contact_request.closed (O) → the audit
// record. The decline
// has no reason field at all: there is nothing the company could be shown.
func (s *service) Respond(ctx context.Context, a authz.Actor, id uuid.UUID, in RespondInput) (StudentView, error) {
	if _, err := s.mine(ctx, a, authz.MessagingContactRequestRespond, id); err != nil {
		return StudentView{}, err
	}
	var to Status
	switch in.Decision {
	case DecisionAccepted:
		to = StatusAccepted
		// 03 §2.3: a lapsed or unverified account grants nothing; students
		// checks it in Grant too (BE-077), this keeps the answer before
		// anything is written.
		if !a.IsVerifiedStudent() {
			return StudentView{}, students.ErrVerificationRequired
		}
	case DecisionDeclined:
		to = StatusDeclined
	default:
		return StudentView{}, problem.Field("body.decision", "must be accepted or declined")
	}
	if err := checkShare(in, to); err != nil {
		return StudentView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		ID      uuid.UUID
		D       Decision
		Share   bool
		Proof   *students.ConsentProof
		Channel string
	}{"respond_contact_request", id, in.Decision, in.ShareContact, shareProof(in), string(shareChannel(in))})
	if err != nil {
		return StudentView{}, err
	}
	if _, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		return s.respond(ctx, a, id, to, in)
	}); err != nil {
		return StudentView{}, err
	}
	return s.GetMine(ctx, a, id)
}

// checkShare is share_contact's rule (T-335): only with an acceptance and
// with the contact_info_share text the student was shown (the consent
// module checks that it is the current text, in the transaction).
func checkShare(in RespondInput, to Status) error {
	switch {
	case !in.ShareContact:
		return nil
	case to != StatusAccepted:
		return problem.Field("body.share_contact", "only with decision accepted")
	case in.ContactConsent == nil:
		return consent.ErrProofRequired.WithFields(problem.FieldError{Location: "body.consent.contact_info_share",
			Message: "the consent text shown is required to share your contact details"})
	}
	return nil
}

// shareProof and shareChannel are the parts of the sharing that matter
// (an unused proof or channel does not change the request).
func shareProof(in RespondInput) *students.ConsentProof {
	if !in.ShareContact {
		return nil
	}
	return in.ContactConsent
}

func shareChannel(in RespondInput) consent.Channel {
	switch {
	case !in.ShareContact:
		return ""
	case in.Channel == consent.ChannelMobile:
		return consent.ChannelMobile
	}
	return consent.ChannelWeb
}

func (s *service) respond(ctx context.Context, a authz.Actor, id uuid.UUID, to Status, in RespondInput) (uuid.UUID, error) {
	cur, err := s.requests().Lock(ctx, id)
	switch {
	case err != nil:
		return uuid.Nil(), err
	case cur.StudentUserID != a.UserID:
		return uuid.Nil(), ErrNotFound
	case cur.Status == to:
		// The same answer again (a retry): nothing changes, a sharing
		// included; sharing later is updateMyPrivacy's.
		return cur.ID, nil
	case !cur.Open(s.now()):
		return uuid.Nil(), ErrInvalidState
	}
	at := s.now()
	next, ok, err := s.requests().Transition(ctx, id, StatusPending, to, &at)
	switch {
	case err != nil:
		return uuid.Nil(), err
	case !ok:
		return uuid.Nil(), ErrInvalidState
	}
	meta := map[string]any{"decision": string(to), "share_contact": in.ShareContact}
	if to == StatusAccepted {
		if err := s.d.Grants.Grant(ctx, a.UserID, next.OrgID, students.SourceContactRequest, next.ID); err != nil {
			return uuid.Nil(), err
		}
		if in.ShareContact {
			if err := s.d.Contacts.ShareContactWithCompany(ctx, a, next.OrgID, *in.ContactConsent, shareChannel(in), next.ID); err != nil {
				return uuid.Nil(), err
			}
		}
		// The conversation of the acceptance (BE-162, Q-40), same transaction.
		c, err := s.openConversation(ctx, next)
		if err != nil {
			return uuid.Nil(), err
		}
		meta["conversation_id"] = c.ID.String()
	}
	if err := s.emit(ctx, domainevent.ContactRequestClosed{
		ContactRequestID: next.ID, OrgID: next.OrgID, Outcome: string(to),
	}, next.ID); err != nil {
		return uuid.Nil(), err
	}
	ev := requestEvent(authz.MessagingContactRequestRespond, next, nil, meta)
	return next.ID, s.d.Audit.Log(ctx, ev.By(a))
}

// ---- blocks (04 §2.9)

// blockable reads the org of a block path: only a company can be blocked;
// anything else is not found (any status: a suspended company is still
// blocked).
func (s *service) blockable(ctx context.Context, orgID uuid.UUID) error {
	o, err := s.d.Orgs.Get(ctx, orgID)
	switch {
	case errors.Is(err, orgs.ErrNotFound):
		return ErrNotFound
	case err != nil:
		return err
	case o.Kind != orgs.KindCompany:
		return ErrNotFound
	}
	return nil
}

// Block implements Service. In one transaction: the block (once), then the
// company's pending requests to the caller are declined (contact_request.closed
// "declined" each: the company cannot tell a block from an ordinary
// decline), a request past its 14 days expires and the open conversations
// with the company close (closed_by student; BE-162). A repeated block with
// nothing pending or open writes nothing.
func (s *service) Block(ctx context.Context, a authz.Actor, orgID uuid.UUID) error {
	if err := authz.Can(ctx, a, authz.MessagingBlockManage, ownBlocks(a)); err != nil {
		return err
	}
	if err := s.blockable(ctx, orgID); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		added, err := s.blocks().InsertBlock(ctx, a.UserID, orgID)
		if err != nil {
			return err
		}
		pending, err := s.requests().PendingOfCompany(ctx, orgID, a.UserID)
		if err != nil {
			return err
		}
		declined := 0
		for _, r := range pending {
			if !r.Open(s.now()) {
				if _, err := s.expire(ctx, r); err != nil {
					return err
				}
				continue
			}
			at := s.now()
			next, ok, err := s.requests().Transition(ctx, r.ID, StatusPending, StatusDeclined, &at)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := s.emit(ctx, domainevent.ContactRequestClosed{
				ContactRequestID: next.ID, OrgID: next.OrgID, Outcome: string(StatusDeclined),
			}, next.ID); err != nil {
				return err
			}
			declined++
		}
		closed, err := s.closeOnBlock(ctx, orgID, a.UserID)
		if err != nil {
			return err
		}
		if !added && declined == 0 && closed == 0 {
			return nil
		}
		ev := blockEvent(a, orgID, true, declined)
		ev.Metadata["closed_conversations"] = closed
		return s.d.Audit.Log(ctx, ev)
	})
}

// Unblock implements Service: the block goes (an absent one is no error).
func (s *service) Unblock(ctx context.Context, a authz.Actor, orgID uuid.UUID) error {
	if err := authz.Can(ctx, a, authz.MessagingBlockManage, ownBlocks(a)); err != nil {
		return err
	}
	if err := s.blockable(ctx, orgID); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		removed, err := s.blocks().DeleteBlock(ctx, a.UserID, orgID)
		if err != nil || !removed {
			return err
		}
		return s.d.Audit.Log(ctx, blockEvent(a, orgID, false, 0))
	})
}

// blockEvent is the audit record of a block change: the student's own
// (no org scope: the company never sees it).
func blockEvent(a authz.Actor, orgID uuid.UUID, blocked bool, declined int) audit.Event {
	return audit.Event{Action: string(authz.MessagingBlockManage), Outcome: audit.Success, ResourceType: authz.TypeContactBlock,
		Metadata: map[string]any{"company_org_id": orgID.String(), "blocked": blocked, "declined_requests": declined}}.By(a)
}

// ListBlocks implements Service.
func (s *service) ListBlocks(ctx context.Context, a authz.Actor, after *BlockKey, limit int) (BlockPage, error) {
	if err := authz.Can(ctx, a, authz.MessagingBlockListOwn, ownBlocks(a)); err != nil {
		return BlockPage{}, err
	}
	limit = page.ClampLimit(limit)
	rows, err := s.blocks().Blocks(ctx, a.UserID, after, limit+1)
	if err != nil {
		return BlockPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	ids := make([]uuid.UUID, len(rows))
	for i, b := range rows {
		ids[i] = b.CompanyOrgID
	}
	names, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return BlockPage{}, err
	}
	out := BlockPage{Items: make([]BlockView, len(rows))}
	for i, b := range rows {
		out.Items[i] = BlockView{Block: b, Company: names[b.CompanyOrgID]}
	}
	if more {
		last := rows[len(rows)-1]
		out.Next = &BlockKey{CreatedAt: last.CreatedAt, OrgID: last.CompanyOrgID}
	}
	return out, nil
}
