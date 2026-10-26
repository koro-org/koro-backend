package parties

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// accountModule and accountAggregate name the bank account row as a files
// owner and as an event aggregate.
const (
	accountModule    = "parties"
	accountAggregate = "payee_bank_account"
)

// accountResource is the authz view of a bank account for the eyes (03
// §3.5 bank_account: Attrs status, submitted_by, verified_by — the earlier
// eyes the actor may not be; 06 H-27).
func accountResource(b BankAccount) authz.Resource {
	org := b.OrgID
	verified := ""
	if b.VerifiedBy != nil {
		verified = b.VerifiedBy.String()
	}
	return authz.Resource{Type: authz.TypeBankAccount, ID: b.ID, OrgID: &org, Attrs: map[string]string{
		authz.AttrStatus: string(b.Status), authz.AttrSubmittedBy: b.SubmittedBy.String(), authz.AttrVerifiedBy: verified,
	}}
}

// accountEvent is the audit record of a bank account action. The IBAN is
// recorded masked only (country, check digits, last six digits).
func accountEvent(a authz.Actor, act authz.Action, b BankAccount, reason string, meta map[string]any) audit.Event {
	id, org := b.ID, b.OrgID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["payee_id"] = b.PayeeID.String()
	meta["iban_masked"] = b.MaskedIBAN()
	meta["status"] = string(b.Status)
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: authz.TypeBankAccount, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// staff reports a finance or platform admin (an admin submission).
func staff(a authz.Actor) bool { return fullTaxIDs(a) }

// reauth verifies the proof of an R operation (03 §6.2) and returns its
// kind for the audit record; without a verifier the operation fails closed.
// A refused proof is audited as denied (denied: reauthDenied); an error
// without a verdict (overload, a dependency down) writes nothing
// (audit.ReauthFailed, BE-074).
func (s *service) reauth(ctx context.Context, a authz.Actor, proof identity.ReauthProof, denied audit.Event) (string, error) {
	if s.d.Reauth == nil {
		return "", ErrUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		return "", audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied.By(a), err)
	}
	return kind, nil
}

// reauthDenied is the denied record of a refused proof of act on a bank
// account of the payee (accountID nil before it exists), scoped to the
// payee's org as the account's own records are. Nothing happened: no IBAN,
// masked or not.
func reauthDenied(act authz.Action, accountID *uuid.UUID, payeeID, orgID uuid.UUID) audit.Event {
	return audit.Event{Action: string(act), OrgID: &orgID, ResourceType: authz.TypeBankAccount, ResourceID: accountID,
		Metadata: map[string]any{"payee_id": payeeID.String()}}
}

// accountViews builds the views of bs: the party title for the first
// eye's check.
func (s *service) accountViews(ctx context.Context, bs []BankAccount) ([]BankAccountView, error) {
	out := make([]BankAccountView, len(bs))
	titles := map[uuid.UUID]string{} // payee → party title
	for i, b := range bs {
		title, ok := titles[b.PayeeID]
		if !ok {
			p, err := s.d.Store.GetPayee(ctx, b.PayeeID, false)
			if err != nil {
				return nil, err
			}
			lp, err := s.d.Store.GetParty(ctx, p.LegalPartyID)
			if err != nil {
				return nil, err
			}
			title = lp.Title
			titles[b.PayeeID] = title
		}
		out[i] = BankAccountView{Account: b, PartyTitle: title, HolderMatchesTitle: holderMatchesTitle(b.HolderName, title)}
	}
	return out, nil
}

func (s *service) accountView(ctx context.Context, b BankAccount) (BankAccountView, error) {
	vs, err := s.accountViews(ctx, []BankAccount{b})
	if err != nil {
		return BankAccountView{}, err
	}
	return vs[0], nil
}

// ---- org surface

// ListPayeeBankAccounts implements Service: the org (404) → authz on the
// org's payees (404/403) → the payee within the org (404) → its accounts,
// newest first, the IBAN masked for everyone (BE-129).
func (s *service) ListPayeeBankAccounts(ctx context.Context, a authz.Actor, orgID, payeeID uuid.UUID) ([]BankAccountView, error) {
	_, p, err := s.orgPayee(ctx, a, authz.PartiesBankAccountList, orgID, payeeID)
	if err != nil {
		return nil, err
	}
	bs, err := s.d.Store.PayeeBankAccounts(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return s.accountViews(ctx, bs)
}

// SubmitPayeeBankAccount implements Service (R). Order: the org (404) →
// authz on the org's payees (404/403; club_admin, university_rep: S) → the
// payee within the org (404) → org status (409/403) → submit.
func (s *service) SubmitPayeeBankAccount(ctx context.Context, a authz.Actor, orgID, payeeID uuid.UUID, in BankAccountInput, proof identity.ReauthProof) (BankAccountView, error) {
	o, p, err := s.orgPayee(ctx, a, authz.PartiesBankAccountSubmit, orgID, payeeID)
	if err != nil {
		return BankAccountView{}, err
	}
	if err := writableOrg(a, o); err != nil {
		return BankAccountView{}, err
	}
	return s.submit(ctx, a, p, in, proof)
}

// AdminSubmitBankAccount implements Service (R): authz (403 before
// reading) → the payee (404) → submit (the submitter is the first eye).
func (s *service) AdminSubmitBankAccount(ctx context.Context, a authz.Actor, payeeID uuid.UUID, in BankAccountInput, proof identity.ReauthProof) (BankAccountView, error) {
	if err := authz.Can(ctx, a, authz.PartiesBankAccountSubmit, adminSurface); err != nil {
		return BankAccountView{}, err
	}
	p, err := s.d.Store.GetPayee(ctx, payeeID, false)
	if err != nil {
		return BankAccountView{}, err
	}
	return s.submit(ctx, a, p, in, proof)
}

// submit is the shared tail of both submissions: payee status (closed: 409)
// → the payee's party (a natural person: 422, 06 H-54) → body (422
// parties.iban_invalid, field errors) → the evidence file → the
// re-authentication proof (403) → in one transaction: the account
// (pending_verification for org roles, pending_approval for finance and
// platform admins; 409 parties.bank_account_pending when one waits), the
// evidence attached, audit, payee_bank_account.submitted (O: finance
// admins and the payee org's admins are told).
func (s *service) submit(ctx context.Context, a authz.Actor, p Payee, in BankAccountInput, proof identity.ReauthProof) (BankAccountView, error) {
	if p.Status == PayeeClosed {
		return BankAccountView{}, ErrInvalidState
	}
	party, err := s.d.Store.GetParty(ctx, p.LegalPartyID)
	if err != nil {
		return BankAccountView{}, err
	}
	if party.LegalType.NaturalPerson() && !s.d.AllowNaturalPersonPayees {
		return BankAccountView{}, ErrNaturalPersonPayee
	}
	number, holder, bank, err := cleanAccountInput(in)
	if err != nil {
		return BankAccountView{}, err
	}
	if s.d.Files == nil {
		return BankAccountView{}, ErrUnavailable
	}
	if err := s.checkEvidence(ctx, a, p.OrgID, in.EvidenceFileID); err != nil {
		return BankAccountView{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.PartiesBankAccountSubmit, nil, p.ID, p.OrgID))
	if err != nil {
		return BankAccountView{}, err
	}
	admin := staff(a)
	b := BankAccount{
		PayeeID: p.ID, OrgID: p.OrgID, IBAN: number, HolderName: holder, BankName: bank, EvidenceFileID: in.EvidenceFileID,
		Status: AccountPendingVerification, SubmittedBy: a.UserID, SubmittedByAdmin: admin,
	}
	if admin {
		b.Status = AccountPendingApproval // the submitting admin is the first eye (03 §6.1)
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		Payee uuid.UUID
		IBAN  string
		Hold  string
		Bank  *string
		File  uuid.UUID
	}{"submit_bank_account", p.ID, number, holder, bank, in.EvidenceFileID})
	if err != nil {
		return BankAccountView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetPayee(ctx, p.ID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status == PayeeClosed {
			return uuid.Nil(), ErrInvalidState
		}
		b.ID, b.SubmittedAt = uuid.NewV7(), s.now()
		stored, err := s.d.Store.InsertBankAccount(ctx, b)
		if err != nil {
			return uuid.Nil(), err
		}
		owner := files.Owner{Module: accountModule, EntityID: stored.ID, Purpose: files.PurposeBankAccountEvidence}
		if err := s.d.Files.Attach(ctx, a, stored.EvidenceFileID, owner); err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{
			"submitted_by_admin": admin, "bank_code": stored.BankCode, "evidence_file_id": stored.EvidenceFileID.String(),
			"holder_matches_title": holderMatchesTitle(stored.HolderName, party.Title), audit.MetaReauth: kind,
		}
		if err := s.d.Audit.Log(ctx, accountEvent(a, authz.PartiesBankAccountSubmit, stored, "", meta)); err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.emit(ctx, stored, domainevent.PayeeBankAccountSubmitted{
			BankAccountID: stored.ID, PayeeID: stored.PayeeID, Stage: string(stored.Status),
		})
	})
	if err != nil {
		return BankAccountView{}, err
	}
	stored, err := s.d.Store.GetBankAccount(ctx, id, false)
	if err != nil {
		return BankAccountView{}, err
	}
	return s.accountView(ctx, stored)
}

// emit writes an outbox event of the bank account flow (O) in the
// transaction carried by ctx.
func (s *service) emit(ctx context.Context, b BankAccount, p domainevent.Payload) error {
	return outbox.Emit(ctx, s.d.Outbox, p, accountAggregate, b.ID)
}

// checkEvidence accepts a bank_account_evidence upload made by the caller
// or for the payee's org (files k30 authorised the upload itself).
func (s *service) checkEvidence(ctx context.Context, a authz.Actor, orgID, fileID uuid.UUID) error {
	bad := problem.Field("body.evidence_file_id", "must be a bank_account_evidence upload of this organization or of the caller")
	f, err := s.d.Files.Get(ctx, fileID)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return bad
	case err != nil:
		return err
	}
	ownOrg := f.OrgID != nil && *f.OrgID == orgID
	ownUpload := f.UploadedBy != nil && *f.UploadedBy == a.UserID
	if f.Purpose != files.PurposeBankAccountEvidence || !ownOrg && !ownUpload {
		return bad
	}
	return nil
}

// ---- admin surface: the four-eyes queue

// queueStatuses is the default filter of adminListBankAccounts.
var queueStatuses = []BankAccountStatus{AccountPendingVerification, AccountPendingApproval}

// AdminListBankAccounts implements Service: authz (403) → the accounts
// waiting for an eye (or of one status), oldest first, with the submitter
// and the first eye; the IBAN masked (BE-129).
func (s *service) AdminListBankAccounts(ctx context.Context, a authz.Actor, q BankAccountQuery) (BankAccountPage, error) {
	if err := authz.Can(ctx, a, authz.PartiesBankAccountList, adminSurface); err != nil {
		return BankAccountPage{}, err
	}
	statuses := queueStatuses
	if q.Status != "" {
		if !q.Status.Valid() {
			return BankAccountPage{}, problem.Field("query.status", "is not a bank account status")
		}
		statuses = []BankAccountStatus{q.Status}
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListBankAccounts(ctx, statuses, q.After, limit+1)
	if err != nil {
		return BankAccountPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	views, err := s.accountViews(ctx, rows)
	if err != nil {
		return BankAccountPage{}, err
	}
	out := BankAccountPage{Items: views}
	if more {
		next := rows[len(rows)-1].ID
		out.Next = &next
	}
	return out, nil
}

// eyesGate is the "403 before reading" check of the eyes' /admin
// operations (03 §7.2-3). verify and approve carry the four-eyes keys of
// the core table on every resource, so they are decided on the account;
// the gate is parties.bank_account.list on the admin surface, whose cells
// are the eyes' (finance and platform admins).
func eyesGate(ctx context.Context, a authz.Actor) error {
	return authz.Can(ctx, a, authz.PartiesBankAccountList, adminSurface)
}

// adminAccount resolves a bank account for an eye: the gate (403 before
// reading) → the account (404) → authz on the account (the four eyes: 403
// authz.four_eyes_violation when the actor is the submitter or, for
// approve, the first eye).
func (s *service) adminAccount(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) (BankAccount, error) {
	if err := eyesGate(ctx, a); err != nil {
		return BankAccount{}, err
	}
	b, err := s.d.Store.GetBankAccount(ctx, id, false)
	if err != nil {
		return BankAccount{}, err
	}
	if err := authz.Can(ctx, a, act, accountResource(b)); err != nil {
		return BankAccount{}, err
	}
	return b, nil
}

// lockAccount locks the account in the transaction carried by ctx, checks
// its status and runs the eyes again on the locked row (a decision between
// the read and the lock cannot slip through).
func (s *service) lockAccount(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID, want ...BankAccountStatus) (BankAccount, error) {
	b, err := s.d.Store.GetBankAccount(ctx, id, true)
	if err != nil {
		return BankAccount{}, err
	}
	ok := false
	for _, w := range want {
		ok = ok || b.Status == w
	}
	if !ok {
		return BankAccount{}, ErrInvalidState
	}
	if err := authz.Can(ctx, a, act, accountResource(b)); err != nil {
		return BankAccount{}, err
	}
	return b, nil
}

// AdminVerifyBankAccount implements Service, the first eye of an org
// submission (03 §6.1): the gate (403) → the checklist (the evidence must
// hold: otherwise reject) → the account (404) → the eyes (403: never the
// submitter) → in one transaction: pending_verification only (409) → the
// note is required when the holder is not the party's title — as the eye
// says or as the folded names differ (T-19) → the evidence must be
// viewable (409 files.not_clean) → pending_approval, verified by and at →
// audit (the checklist; the note as the reason) →
// payee_bank_account.submitted (stage pending_approval: the second eye is
// called).
func (s *service) AdminVerifyBankAccount(ctx context.Context, a authz.Actor, id uuid.UUID, in VerifyInput) (BankAccountView, error) {
	if err := eyesGate(ctx, a); err != nil {
		return BankAccountView{}, err
	}
	if !in.EvidenceOK {
		return BankAccountView{}, problem.Field("body.checklist.evidence_ok",
			"must be true: an account whose evidence does not hold is rejected (adminRejectBankAccount)")
	}
	note, err := cleanNote("body.note", in.Note)
	if err != nil {
		return BankAccountView{}, err
	}
	if _, err := s.adminAccount(ctx, a, authz.PartiesBankAccountVerify, id); err != nil {
		return BankAccountView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Account uuid.UUID
		In      VerifyInput
	}{"verify_bank_account", id, VerifyInput{HolderMatchesParty: in.HolderMatchesParty, EvidenceOK: in.EvidenceOK, Note: note}})
	if err != nil {
		return BankAccountView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.lockAccount(ctx, a, authz.PartiesBankAccountVerify, id, AccountPendingVerification)
		if err != nil {
			return uuid.Nil(), err
		}
		v, err := s.accountView(ctx, cur)
		if err != nil {
			return uuid.Nil(), err
		}
		if (!in.HolderMatchesParty || !v.HolderMatchesTitle) && note == "" {
			return uuid.Nil(), problem.Field("body.note",
				"is required when the account holder is not the legal party's title (T-19)")
		}
		if err := s.checkReviewable(ctx, cur.EvidenceFileID); err != nil {
			return uuid.Nil(), err
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.VerifiedBy, next.VerifiedAt = AccountPendingApproval, &by, &now
		stored, err := s.d.Store.TransitionBankAccount(ctx, next, AccountPendingVerification)
		if err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{"from": string(cur.Status), "holder_matches_party": in.HolderMatchesParty,
			"holder_matches_title": v.HolderMatchesTitle, "evidence_ok": in.EvidenceOK}
		if err := s.d.Audit.Log(ctx, accountEvent(a, authz.PartiesBankAccountVerify, stored, note, meta)); err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.emit(ctx, stored, domainevent.PayeeBankAccountSubmitted{
			BankAccountID: stored.ID, PayeeID: stored.PayeeID, Stage: string(stored.Status),
		})
	})
	if err != nil {
		return BankAccountView{}, err
	}
	return s.reloadAccount(ctx, id)
}

// AdminApproveBankAccount implements Service, the second eye (03 §6.1, R):
// authz (403) → the account (404) → the eyes (403: never the submitter or
// the first eye) → the re-authentication proof (403) → in one transaction:
// pending_approval only (409) → the payee, locked (closed: 409) → the
// evidence must be viewable (409 files.not_clean) → the payee's approved
// account, if any, becomes superseded by this one, then
// this one approved (one approved account per payee at any time) → audit →
// payee_bank_account.approved (O: the security notice to the payee org's
// admins and every finance admin).
func (s *service) AdminApproveBankAccount(ctx context.Context, a authz.Actor, id uuid.UUID, proof identity.ReauthProof) (BankAccountView, error) {
	b, err := s.adminAccount(ctx, a, authz.PartiesBankAccountApprove, id)
	if err != nil {
		return BankAccountView{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.PartiesBankAccountApprove, &b.ID, b.PayeeID, b.OrgID))
	if err != nil {
		return BankAccountView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Account uuid.UUID
	}{"approve_bank_account", id})
	if err != nil {
		return BankAccountView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.lockAccount(ctx, a, authz.PartiesBankAccountApprove, id, AccountPendingApproval)
		if err != nil {
			return uuid.Nil(), err
		}
		p, err := s.d.Store.GetPayee(ctx, cur.PayeeID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if p.Status == PayeeClosed {
			return uuid.Nil(), ErrInvalidState
		}
		// The second eye looks at the evidence too (an admin submission has no
		// first eye that did): it must be viewable.
		if err := s.checkReviewable(ctx, cur.EvidenceFileID); err != nil {
			return uuid.Nil(), err
		}
		now, by := s.now(), a.UserID
		meta := map[string]any{"from": string(cur.Status), audit.MetaReauth: kind,
			"submitted_by_admin": cur.SubmittedByAdmin}
		old, err := s.d.Store.ApprovedAccounts(ctx, []uuid.UUID{cur.PayeeID}, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if prev, ok := old[cur.PayeeID]; ok {
			sup := prev
			sup.Status, sup.SupersededAt, sup.SupersededByID = AccountSuperseded, &now, &cur.ID
			if _, err := s.d.Store.TransitionBankAccount(ctx, sup, AccountApproved); err != nil {
				return uuid.Nil(), err
			}
			meta["superseded_bank_account_id"] = prev.ID.String()
			meta["superseded_iban_masked"] = prev.MaskedIBAN()
		}
		next := cur
		next.Status, next.ApprovedBy, next.ApprovedAt = AccountApproved, &by, &now
		stored, err := s.d.Store.TransitionBankAccount(ctx, next, AccountPendingApproval)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, accountEvent(a, authz.PartiesBankAccountApprove, stored, "", meta)); err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.emit(ctx, stored, domainevent.PayeeBankAccountApproved{BankAccountID: stored.ID, PayeeID: stored.PayeeID})
	})
	if err != nil {
		return BankAccountView{}, err
	}
	return s.reloadAccount(ctx, id)
}

// AdminRejectBankAccount implements Service: authz (403) → the reason
// (required) → in one transaction: the account, locked (404), waiting for
// an eye (409) → rejected by, at, with the reason → audit →
// payee_bank_account.rejected (A: the submitter is told).
func (s *service) AdminRejectBankAccount(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (BankAccountView, error) {
	if err := authz.Can(ctx, a, authz.PartiesBankAccountReject, adminSurface); err != nil {
		return BankAccountView{}, err
	}
	why, err := cleanReason("body.rejection_reason", reason, 1)
	if err != nil {
		return BankAccountView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Account uuid.UUID
		Reason  string
	}{"reject_bank_account", id, why})
	if err != nil {
		return BankAccountView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetBankAccount(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if !cur.Status.Pending() {
			return uuid.Nil(), ErrInvalidState
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.RejectedBy, next.RejectedAt, next.RejectionReason = AccountRejected, &by, &now, &why
		stored, err := s.d.Store.TransitionBankAccount(ctx, next, cur.Status)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, accountEvent(a, authz.PartiesBankAccountReject, stored, why,
			map[string]any{"from": string(cur.Status)})); err != nil {
			return uuid.Nil(), err
		}
		env, err := domainevent.New(domainevent.PayeeBankAccountRejected{BankAccountID: stored.ID, PayeeID: stored.PayeeID},
			accountAggregate, stored.ID)
		if err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.d.Outbox.AfterCommit(ctx, env)
	})
	if err != nil {
		return BankAccountView{}, err
	}
	return s.reloadAccount(ctx, id)
}

// AdminRevokeBankAccount implements Service (R): authz (403) → the reason
// (required) → the account (404) → the re-authentication proof (403) → in
// one transaction: approved only (409; a pending one is rejected) →
// revoked by, at, with the reason → audit → the revocation notice (A).
// Money stops at once: no payout may name a revoked account
// (payouts_bank_account_guard); a payout created before stays to be
// cancelled (04 §2.13).
func (s *service) AdminRevokeBankAccount(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof identity.ReauthProof) (BankAccountView, error) {
	if err := authz.Can(ctx, a, authz.PartiesBankAccountRevoke, adminSurface); err != nil {
		return BankAccountView{}, err
	}
	why, err := cleanReason("body.revoke_reason", reason, 1)
	if err != nil {
		return BankAccountView{}, err
	}
	b, err := s.d.Store.GetBankAccount(ctx, id, false)
	if err != nil {
		return BankAccountView{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.PartiesBankAccountRevoke, &b.ID, b.PayeeID, b.OrgID))
	if err != nil {
		return BankAccountView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Account uuid.UUID
		Reason  string
	}{"revoke_bank_account", id, why})
	if err != nil {
		return BankAccountView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetBankAccount(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != AccountApproved {
			return uuid.Nil(), ErrInvalidState
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.RevokedBy, next.RevokedAt, next.RevokeReason = AccountRevoked, &by, &now, &why
		stored, err := s.d.Store.TransitionBankAccount(ctx, next, AccountApproved)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, accountEvent(a, authz.PartiesBankAccountRevoke, stored, why,
			map[string]any{"from": string(cur.Status), audit.MetaReauth: kind})); err != nil {
			return uuid.Nil(), err
		}
		s.d.Queue.AfterCommit(ctx, NotifyRevokedTaskName, accountNotice{BankAccountID: stored.ID, PayeeID: stored.PayeeID},
			NotifyRevokedTaskName+":"+stored.ID.String())
		return stored.ID, nil
	})
	if err != nil {
		return BankAccountView{}, err
	}
	return s.reloadAccount(ctx, id)
}

// AdminDownloadBankAccountEvidence implements Service: authz (403) → the
// account (404) → a 5-minute URL of a clean evidence file (409
// files.not_clean) → audit (every download, before the URL leaves).
func (s *service) AdminDownloadBankAccountEvidence(ctx context.Context, a authz.Actor, id uuid.UUID) (DocumentLink, error) {
	if err := authz.Can(ctx, a, authz.PartiesBankAccountList, adminSurface); err != nil {
		return DocumentLink{}, err
	}
	b, err := s.d.Store.GetBankAccount(ctx, id, false)
	if err != nil {
		return DocumentLink{}, err
	}
	if s.d.Files == nil {
		return DocumentLink{}, ErrUnavailable
	}
	dl, err := s.d.Files.PresignGet(ctx, b.EvidenceFileID, docTTL)
	if err != nil {
		return DocumentLink{}, err
	}
	meta := map[string]any{"operation": "evidence_download", "file_id": b.EvidenceFileID.String(),
		"ttl_seconds": int(docTTL.Seconds())}
	if err := s.d.Audit.Log(ctx, accountEvent(a, authz.PartiesBankAccountList, b, "", meta)); err != nil {
		return DocumentLink{}, err
	}
	return DocumentLink{URL: dl.URL, ExpiresAt: dl.ExpiresAt}, nil
}

// AdminRevealBankAccountIBAN implements Service (BE-129): authz (403
// before reading) → the account (404) → the access reason (422) and the
// audit record (RevealIBAN) → its full IBAN.
func (s *service) AdminRevealBankAccountIBAN(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (string, error) {
	if err := authz.Can(ctx, a, authz.PartiesIbanReveal, adminSurface); err != nil {
		return "", err
	}
	b, err := s.d.Store.GetBankAccount(ctx, id, false)
	if err != nil {
		return "", err
	}
	org := b.OrgID
	return RevealIBAN(ctx, s.d.Audit, a, Revealed{Action: authz.PartiesIbanReveal, ResourceType: authz.TypeBankAccount,
		ResourceID: b.ID, OrgID: &org, Field: "iban", IBAN: b.IBAN}, reason)
}

func (s *service) reloadAccount(ctx context.Context, id uuid.UUID) (BankAccountView, error) {
	b, err := s.d.Store.GetBankAccount(ctx, id, false)
	if err != nil {
		return BankAccountView{}, err
	}
	return s.accountView(ctx, b)
}
