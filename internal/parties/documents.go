package parties

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// documentModule is the files owner module of the documents.
const documentModule = "parties"

// UploadDocument implements Service. Order: the org (404) → authz on the
// org's collection (404/403: no ID probing) → the party within the org
// (404) → authz on the party (k21) → org status (403) → party status (409)
// → body → the file: a party_document upload of the caller or of this org
// (422 otherwise) → in one transaction: the document row, files.Attach,
// audit.
func (s *service) UploadDocument(ctx context.Context, a authz.Actor, orgID, partyID uuid.UUID, in DocumentInput) (Document, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return Document{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesDocumentUpload, orgListResource(orgID)); err != nil {
		return Document{}, err
	}
	op, err := s.d.Store.OrgParty(ctx, orgID, partyID, false)
	if err != nil {
		return Document{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesDocumentUpload, partyResource(orgID, op)); err != nil {
		return Document{}, err
	}
	if err := writableOrg(a, o); err != nil {
		return Document{}, err
	}
	if op.Party.Status == StatusInactive {
		return Document{}, ErrInvalidState
	}
	if !in.Kind.Valid() {
		return Document{}, problem.Field("body.kind", "is not a document kind")
	}
	if s.d.Files == nil {
		return Document{}, problem.ErrUnavailable
	}
	if err := s.checkUpload(ctx, a, orgID, in.FileID); err != nil {
		return Document{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op             string
		Org, Party, By uuid.UUID
		Kind           DocumentKind
		File           uuid.UUID
	}{"upload_document", orgID, partyID, a.UserID, in.Kind, in.FileID})
	if err != nil {
		return Document{}, err
	}
	org := orgID
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.OrgParty(ctx, orgID, partyID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Party.Status == StatusInactive {
			return uuid.Nil(), ErrInvalidState
		}
		d, err := s.d.Store.InsertDocument(ctx, Document{
			ID: uuid.NewV7(), LegalPartyID: partyID, Kind: in.Kind, FileID: in.FileID, UploadedBy: a.UserID,
		})
		if err != nil {
			return uuid.Nil(), err
		}
		owner := files.Owner{Module: documentModule, EntityID: d.ID, Purpose: files.PurposePartyDocument}
		if err := s.d.Files.Attach(ctx, a, in.FileID, owner); err != nil {
			return uuid.Nil(), err
		}
		return d.ID, s.d.Audit.Log(ctx, documentEvent(a, authz.PartiesDocumentUpload, &org, cur.Party, d, nil))
	})
	if err != nil {
		return Document{}, err
	}
	return s.d.Store.GetDocument(ctx, id, false)
}

// checkUpload accepts a party_document upload made by the caller or for
// this org (files k30 authorised the upload itself; 04 §1.14).
func (s *service) checkUpload(ctx context.Context, a authz.Actor, orgID, fileID uuid.UUID) error {
	bad := problem.Field("body.file_id", "must be a party_document upload of this organization")
	f, err := s.d.Files.Get(ctx, fileID)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return bad
	case err != nil:
		return err
	}
	ownOrg := f.OrgID != nil && *f.OrgID == orgID
	ownUpload := f.UploadedBy != nil && *f.UploadedBy == a.UserID
	if f.Purpose != files.PurposePartyDocument || !ownOrg && !ownUpload {
		return bad
	}
	return nil
}

// DownloadDocument implements Service. Order: the org (404) → authz on the
// org's collection (404/403: no ID probing) → the party within the org
// (404) → authz on the party (k21) → the party's document (404) → a
// 5-minute URL of a clean file (409 files.not_clean otherwise) → audit
// (every download, before the URL is handed out).
func (s *service) DownloadDocument(ctx context.Context, a authz.Actor, orgID, partyID, documentID uuid.UUID) (DocumentLink, error) {
	if _, err := s.org(ctx, orgID); err != nil {
		return DocumentLink{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesDocumentRead, orgListResource(orgID)); err != nil {
		return DocumentLink{}, err
	}
	op, err := s.d.Store.OrgParty(ctx, orgID, partyID, false)
	if err != nil {
		return DocumentLink{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesDocumentRead, partyResource(orgID, op)); err != nil {
		return DocumentLink{}, err
	}
	d, err := s.d.Store.GetDocument(ctx, documentID, false)
	if err != nil {
		return DocumentLink{}, err
	}
	if d.LegalPartyID != partyID {
		return DocumentLink{}, ErrNotFound
	}
	if s.d.Files == nil {
		return DocumentLink{}, problem.ErrUnavailable
	}
	dl, err := s.d.Files.PresignGet(ctx, d.FileID, docTTL)
	if err != nil {
		return DocumentLink{}, err
	}
	org := orgID
	meta := map[string]any{"operation": "download", "ttl_seconds": int(docTTL.Seconds())}
	if err := s.d.Audit.Log(ctx, documentEvent(a, authz.PartiesDocumentRead, &org, op.Party, d, meta)); err != nil {
		return DocumentLink{}, err
	}
	return DocumentLink{URL: dl.URL, ExpiresAt: dl.ExpiresAt}, nil
}

// AdminVerifyDocument implements Service: authz on the admin surface (403
// before reading) → in one transaction: the document, locked (404) → authz
// on the document (the uploader may not verify it: 403
// authz.four_eyes_violation) → 409 when it is verified already → the file
// must be usable (an infected or unscanned file is 409 files.not_clean, a
// rejected one 409 files.rejected: nobody could have looked at it) →
// verified by and at → audit.
func (s *service) AdminVerifyDocument(ctx context.Context, a authz.Actor, documentID uuid.UUID) (Document, error) {
	if err := authz.Can(ctx, a, authz.PartiesDocumentVerify, adminSurface); err != nil {
		return Document{}, err
	}
	var out Document
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		d, err := s.d.Store.GetDocument(ctx, documentID, true)
		if err != nil {
			return err
		}
		if err := authz.Can(ctx, a, authz.PartiesDocumentVerify, documentResource(d)); err != nil {
			return err
		}
		if d.VerifiedAt != nil {
			return ErrInvalidState
		}
		if err := s.checkReviewable(ctx, d.FileID); err != nil {
			return err
		}
		p, err := s.d.Store.GetParty(ctx, d.LegalPartyID)
		if err != nil {
			return err
		}
		if out, err = s.d.Store.SetDocumentVerified(ctx, d.ID, a.UserID, s.now()); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, documentEvent(a, authz.PartiesDocumentVerify, nil, p, out, nil))
	})
	if err != nil {
		return Document{}, err
	}
	return out, nil
}

// checkReviewable reports whether the file of a document can be looked at:
// active and scanned clean (or skipped while no scanner is wired, the
// files rule of PresignGet).
func (s *service) checkReviewable(ctx context.Context, fileID uuid.UUID) error {
	if s.d.Files == nil {
		return problem.ErrUnavailable
	}
	f, err := s.d.Files.Get(ctx, fileID)
	if err != nil {
		return err
	}
	switch {
	case f.Status == files.StatusRejected:
		return files.ErrRejected
	case f.Status != files.StatusActive || f.ScanStatus != files.ScanClean && f.ScanStatus != files.ScanSkipped:
		return files.ErrNotClean
	}
	return nil
}

// documentEvent is the audit record of a document action: the document is
// the resource; the party, the file and the kind go to the metadata.
func documentEvent(a authz.Actor, act authz.Action, orgID *uuid.UUID, p LegalParty, d Document, meta map[string]any) audit.Event {
	if meta == nil {
		meta = map[string]any{}
	}
	meta["legal_party_id"] = p.ID.String()
	meta["party_kind"] = string(p.Kind)
	meta["document_kind"] = string(d.Kind)
	meta["file_id"] = d.FileID.String()
	if orgID == nil {
		orgID = p.OrgID
	}
	id := d.ID
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: orgID, ResourceType: auditDocument, ResourceID: &id,
		Metadata: meta,
	}.By(a)
}
