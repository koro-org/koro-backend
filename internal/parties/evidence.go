package parties

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// Rejected evidence of the legal parties (T-393, T-397; BE-154b). A party
// document or a bank account's evidence that the scan rejects after its
// upload stays referenced by its row (a legal or financial record,
// BE-151; the old file row stays attached as its history): the row shows
// the file's state, the people who upload it are told, and the cell of the
// first upload attaches a new file in its place (no new action; 000163
// koro_evidence_file_guard lets only a rejected file go).
//
//   - A document's new file is unverified: the replacer is its uploader
//     (the verifier may not be, 06 H-60) and a verification is reset.
//   - A bank account's evidence is replaced while the account waits for an
//     eye only (an approved account is submitted again: a new row through
//     four eyes). An org submission waiting for the second eye goes back to
//     the first (the first eye looked at the old evidence); an org role
//     replaces an org submission only (the submitting admin of an admin
//     submission vouched for its evidence as its first eye). The IBAN, the
//     holder and the submitter never change; no eye may vouch for evidence
//     it uploaded (verify and approve refuse the evidence's uploader).

// EvidenceService is the replacement of a rejected document file or
// bank account evidence (BE-154b).
type EvidenceService interface {
	// ReplaceDocumentFile is replaceLegalPartyDocumentFile (org surface,
	// parties.document.upload).
	ReplaceDocumentFile(ctx context.Context, a authz.Actor, orgID, partyID, documentID, fileID uuid.UUID) (DocumentView, error)
	// AdminReplaceDocumentFile is adminReplaceLegalPartyDocumentFile.
	AdminReplaceDocumentFile(ctx context.Context, a authz.Actor, documentID, fileID uuid.UUID) (DocumentView, error)
	// ReplaceBankAccountEvidence is replacePayeeBankAccountEvidence (org
	// surface, parties.bank_account.submit, R).
	ReplaceBankAccountEvidence(ctx context.Context, a authz.Actor, orgID, payeeID, accountID, fileID uuid.UUID,
		proof identity.ReauthProof) (BankAccountView, error)
	// DocumentFileStates returns the states of the files of ids that exist
	// (one files read): DocumentDTO.file_state of the document responses.
	DocumentFileStates(ctx context.Context, ids ...*uuid.UUID) (map[uuid.UUID]files.State, error)
}

// DocumentView is a document with the state of its file.
type DocumentView struct {
	Document
	FileState files.State
}

// fileStates reads the states of ids (no files port: no states).
func (s *service) fileStates(ctx context.Context, ids ...*uuid.UUID) (map[uuid.UUID]files.State, error) {
	if s.d.Files == nil {
		return map[uuid.UUID]files.State{}, nil
	}
	return s.d.Files.States(ctx, files.IDs(ids...))
}

// DocumentFileStates implements EvidenceService.
func (s *service) DocumentFileStates(ctx context.Context, ids ...*uuid.UUID) (map[uuid.UUID]files.State, error) {
	return s.fileStates(ctx, ids...)
}

// documentView reads a document back with the state of its file.
func (s *service) documentView(ctx context.Context, id uuid.UUID) (DocumentView, error) {
	d, err := s.d.Store.GetDocument(ctx, id, false)
	if err != nil {
		return DocumentView{}, err
	}
	st, err := s.fileStates(ctx, &d.FileID)
	if err != nil {
		return DocumentView{}, err
	}
	return DocumentView{Document: d, FileState: files.StateOf(st, &d.FileID)}, nil
}

// fileRequired is the 422 of a missing new file.
func fileRequired(loc, purpose string, fileID uuid.UUID) error {
	if fileID == uuid.Nil() {
		return problem.Field(loc, "is required: the new "+purpose+" upload")
	}
	return nil
}

// ReplaceDocumentFile implements EvidenceService, the checks of
// UploadDocument: the org (404) → authz on its collection (404/403) → the
// party within the org (404) → authz on the party (k21) → org status (403)
// → party status (409) → the new file (a party_document upload of the
// caller or of this org; 422) → in one transaction: the document of the
// party, locked (404) → the replacement (409 files.not_rejected …) →
// audit.
func (s *service) ReplaceDocumentFile(ctx context.Context, a authz.Actor, orgID, partyID, documentID, fileID uuid.UUID) (DocumentView, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return DocumentView{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesDocumentUpload, orgListResource(orgID)); err != nil {
		return DocumentView{}, err
	}
	op, err := s.d.Store.OrgParty(ctx, orgID, partyID, false)
	if err != nil {
		return DocumentView{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesDocumentUpload, partyResource(orgID, op)); err != nil {
		return DocumentView{}, err
	}
	if err := writableOrg(a, o); err != nil {
		return DocumentView{}, err
	}
	if op.Party.Status == StatusInactive {
		return DocumentView{}, ErrInvalidState
	}
	if err := fileRequired("body.file_id", string(files.PurposePartyDocument), fileID); err != nil {
		return DocumentView{}, err
	}
	if s.d.Files == nil {
		return DocumentView{}, problem.ErrUnavailable
	}
	if err := s.checkUpload(ctx, a, orgID, fileID); err != nil {
		return DocumentView{}, err
	}
	org := orgID
	return s.replaceDocument(ctx, a, documentID, fileID, &partyID, &org)
}

// AdminReplaceDocumentFile implements EvidenceService, the checks of
// AdminUploadDocument: authz (403 before reading) → the new file (a
// party_document upload of the caller; 422) → in one transaction: the
// document, locked (404) → its party active (409) → the replacement →
// audit.
func (s *service) AdminReplaceDocumentFile(ctx context.Context, a authz.Actor, documentID, fileID uuid.UUID) (DocumentView, error) {
	if err := authz.Can(ctx, a, authz.PartiesDocumentUpload, adminSurface); err != nil {
		return DocumentView{}, err
	}
	if err := fileRequired("body.file_id", string(files.PurposePartyDocument), fileID); err != nil {
		return DocumentView{}, err
	}
	if s.d.Files == nil {
		return DocumentView{}, ErrUnavailable
	}
	if err := s.checkOwnUpload(ctx, a, fileID, "body.file_id"); err != nil {
		return DocumentView{}, err
	}
	return s.replaceDocument(ctx, a, documentID, fileID, nil, nil)
}

// replaceDocument is the transaction of a document's replacement (with the
// Idempotency-Key): the document locked (404; of partyID when given) → its
// party active (409) → files.Replace → the new file, the replacer as its
// uploader, the verification reset → audit (the old and the new file, the
// reset verification).
func (s *service) replaceDocument(ctx context.Context, a authz.Actor, documentID, fileID uuid.UUID, partyID, orgID *uuid.UUID) (DocumentView, error) {
	hash, err := idempotency.Hash(struct {
		Op             string
		Document, File uuid.UUID
	}{"replace_document_file", documentID, fileID})
	if err != nil {
		return DocumentView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetDocument(ctx, documentID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if partyID != nil && cur.LegalPartyID != *partyID {
			return uuid.Nil(), ErrNotFound
		}
		p, err := s.d.Store.GetParty(ctx, cur.LegalPartyID)
		if err != nil {
			return uuid.Nil(), err
		}
		if p.Status == StatusInactive {
			return uuid.Nil(), ErrInvalidState
		}
		owner := files.Owner{Module: documentModule, EntityID: cur.ID, Purpose: files.PurposePartyDocument}
		meta, err := files.Replace(ctx, s.d.Files, a, cur.FileID, fileID, owner)
		if err != nil {
			return uuid.Nil(), err
		}
		stored, err := s.d.Store.ReplaceDocumentFile(ctx, cur.ID, cur.FileID, fileID, a.UserID)
		if err != nil {
			return uuid.Nil(), err
		}
		meta["verification_reset"] = cur.VerifiedAt != nil
		if orgID == nil {
			meta["surface"] = "admin"
		}
		return stored.ID, s.d.Audit.Log(ctx, documentEvent(a, authz.PartiesDocumentUpload, orgID, p, stored, meta))
	})
	if err != nil {
		return DocumentView{}, err
	}
	return s.documentView(ctx, documentID)
}

// ReplaceBankAccountEvidence implements EvidenceService, the checks of
// SubmitPayeeBankAccount (R): the org (404) → authz on the org's payees
// (404/403) → the payee within the org (404) → org status (403) → payee
// status (409) → the new evidence (a bank_account_evidence upload of this
// org or of the caller; 422) → the payee's account (404) waiting for an
// eye, and for an org role an org submission (409 parties.invalid_state)
// → the re-authentication proof (403) → in one transaction: the account
// locked and checked again → the replacement → an org submission waiting
// for the second eye goes back to the first → audit.
func (s *service) ReplaceBankAccountEvidence(ctx context.Context, a authz.Actor, orgID, payeeID, accountID, fileID uuid.UUID,
	proof identity.ReauthProof) (BankAccountView, error) {
	o, p, err := s.orgPayee(ctx, a, authz.PartiesBankAccountSubmit, orgID, payeeID)
	if err != nil {
		return BankAccountView{}, err
	}
	if err := writableOrg(a, o); err != nil {
		return BankAccountView{}, err
	}
	if p.Status == PayeeClosed {
		return BankAccountView{}, ErrInvalidState
	}
	if err := fileRequired("body.evidence_file_id", string(files.PurposeBankAccountEvidence), fileID); err != nil {
		return BankAccountView{}, err
	}
	if s.d.Files == nil {
		return BankAccountView{}, ErrUnavailable
	}
	if err := s.checkEvidence(ctx, a, p.OrgID, fileID); err != nil {
		return BankAccountView{}, err
	}
	b, err := s.d.Store.GetBankAccount(ctx, accountID, false)
	if err != nil {
		return BankAccountView{}, err
	}
	if b.PayeeID != p.ID {
		return BankAccountView{}, ErrNotFound
	}
	if err := evidenceReplaceable(a, b); err != nil {
		return BankAccountView{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.PartiesBankAccountSubmit, &b.ID, p.ID, p.OrgID))
	if err != nil {
		return BankAccountView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op            string
		Account, File uuid.UUID
	}{"replace_bank_account_evidence", accountID, fileID})
	if err != nil {
		return BankAccountView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetBankAccount(ctx, accountID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := evidenceReplaceable(a, cur); err != nil {
			return uuid.Nil(), err
		}
		owner := files.Owner{Module: accountModule, EntityID: cur.ID, Purpose: files.PurposeBankAccountEvidence}
		meta, err := files.Replace(ctx, s.d.Files, a, cur.EvidenceFileID, fileID, owner)
		if err != nil {
			return uuid.Nil(), err
		}
		next := cur
		next.EvidenceFileID = fileID
		if cur.Status == AccountPendingApproval && !cur.SubmittedByAdmin {
			next.Status, next.VerifiedBy, next.VerifiedAt = AccountPendingVerification, nil, nil
		}
		stored, err := s.d.Store.ReplaceAccountEvidence(ctx, next, cur.EvidenceFileID, cur.Status)
		if err != nil {
			return uuid.Nil(), err
		}
		meta["from"], meta[audit.MetaReauth] = string(cur.Status), kind
		return stored.ID, s.d.Audit.Log(ctx, accountEvent(a, authz.PartiesBankAccountSubmit, stored, "", meta))
	})
	if err != nil {
		return BankAccountView{}, err
	}
	return s.reloadAccount(ctx, accountID)
}

// evidenceReplaceable checks that a's replacement of b's evidence keeps the
// eyes sound: b waits for an eye (an approved account is submitted again)
// and, for an org role, b is an org submission (409 parties.invalid_state).
func evidenceReplaceable(a authz.Actor, b BankAccount) error {
	if !b.Status.Pending() || b.SubmittedByAdmin && !staff(a) {
		return ErrInvalidState
	}
	return nil
}

// notAnEvidenceUploader refuses an eye of a bank account who uploaded its
// evidence (403 authz.four_eyes_violation): whoever supplied the evidence
// vouches for it as its submitter does (BE-154b).
func notAnEvidenceUploader(a authz.Actor, f files.FileMeta) error {
	if f.UploadedBy != nil && *f.UploadedBy == a.UserID {
		return authz.ErrFourEyes
	}
	return nil
}

// ---- file.rejected

// NotifyRejectedEvidenceTaskName is the file.rejected subscription of the
// party documents and the bank account evidence (BE-154b, T-397).
const NotifyRejectedEvidenceTaskName = "parties.notify_rejected_evidence"

// Templates of the rejected evidence notices (in-app; transactional,
// preference orders_payments).
const (
	// tmplAccountEvidenceRejected: the payee org's admins and the finance
	// admins (an account that waits for an eye or is approved).
	tmplAccountEvidenceRejected = "parties.bank_account_evidence_rejected"
	// tmplDocumentRejected: the finance admins and the admins of the
	// party's org (a buyer company, a university's own party).
	tmplDocumentRejected = "parties.document_rejected"
)

// notifyRejectedEvidence tells the people who upload a rejected document
// or bank account evidence again (the row keeps the reference): for an
// account that waits for an eye or is approved, the payee org's admins
// (their payee) and the finance admins (the four-eyes queue); for a
// document, the finance admins and the admins of the party's org. A row
// that holds another file by now (replaced) and a closed account are
// silent; the dedupe keys (the file and the recipient) make a repeated
// delivery one notice. The uploader hears from files as before.
func (s *service) notifyRejectedEvidence(ctx context.Context, r files.Rejection) error {
	key := r.FileID.String()
	switch r.Purpose {
	case files.PurposeBankAccountEvidence:
		b, err := s.d.Store.GetBankAccount(ctx, r.EntityID, false)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		case b.EvidenceFileID != r.FileID || !b.Status.Pending() && b.Status != AccountApproved:
			return nil
		}
		fin, err := s.financeAdmins(ctx)
		if err != nil {
			return err
		}
		admins, err := s.orgAdmins(ctx, b.OrgID)
		if err != nil {
			return err
		}
		org, data := b.OrgID, map[string]any{"bank_account_id": b.ID.String(), "payee_id": b.PayeeID.String()}
		return errors.Join(
			s.notifyInApp(ctx, tmplAccountEvidenceRejected, key, fin, notify.AdminBankAccountsLink(), &org, data),
			s.notifyInApp(ctx, tmplAccountEvidenceRejected, key, admins, notify.OrgPayeeLink(b.OrgID, b.PayeeID), &org, data))
	case files.PurposePartyDocument:
		d, err := s.d.Store.GetDocument(ctx, r.EntityID, false)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		case d.FileID != r.FileID:
			return nil
		}
		p, err := s.d.Store.GetParty(ctx, d.LegalPartyID)
		if err != nil {
			return err
		}
		to, err := s.financeAdmins(ctx)
		if err != nil {
			return err
		}
		if p.OrgID != nil {
			roles := []authz.MemberRole{authz.MemberClubAdmin, authz.MemberUniversityRep}
			if p.Kind == KindBuyerCompany {
				roles = []authz.MemberRole{authz.MemberCompanyAdmin, authz.MemberCompanyOwner}
			}
			admins, err := s.membersWith(ctx, *p.OrgID, roles...)
			if err != nil {
				return err
			}
			for _, id := range admins {
				if !slices.Contains(to, id) {
					to = append(to, id)
				}
			}
		}
		data := map[string]any{"document_id": d.ID.String(), "legal_party_id": p.ID.String()}
		return s.notifyInApp(ctx, tmplDocumentRejected, key, to, notify.Link{}, p.OrgID, data)
	}
	return nil
}

// membersWith returns the active members of an org holding one of roles.
func (s *service) membersWith(ctx context.Context, orgID uuid.UUID, roles ...authz.MemberRole) ([]uuid.UUID, error) {
	ids, err := s.d.Orgs.ActiveMemberIDs(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("parties: org members: %w", err)
	}
	var out []uuid.UUID
	for _, id := range ids {
		ok, err := s.d.Orgs.HasRole(ctx, orgID, id, roles...)
		if err != nil {
			return nil, fmt.Errorf("parties: org member role: %w", err)
		}
		if ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// notifyInApp sends an in-app notice (IDs only) to every recipient; the
// dedupe key (the event and the recipient) makes a repeated task a no-op
// and a permanent failure for one recipient is logged and skipped.
func (s *service) notifyInApp(ctx context.Context, tmpl, key string, to []uuid.UUID, link notify.Link, org *uuid.UUID,
	data map[string]any) error {
	if len(to) == 0 {
		return nil
	}
	if s.d.Sender == nil {
		return errors.New("parties: notify is not wired")
	}
	var errs []error
	for _, uid := range to {
		u := uid
		_, err := s.d.Sender.Send(ctx, notify.Message{
			UserID: &u, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
			SourceOrgID: org, LinkPath: link, DedupeKey: tmpl + ":" + key + ":" + u.String() + ":in_app", Data: data,
		})
		if err != nil && !notify.IsPermanent(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
