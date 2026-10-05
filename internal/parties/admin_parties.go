package parties

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/idempotency"
)

// ownershipResource is the authz view of an ownership correction (06
// H-60): a platform admin's decision only (authz.TypeLegalPartyOwnership).
func ownershipResource(partyID uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeLegalPartyOwnership, ID: partyID}
}

// AdminGetParty implements Service (T-234): authz (403 before reading) →
// the party with its documents and ETag (the If-Match of
// adminUpdateLegalParty).
func (s *service) AdminGetParty(ctx context.Context, a authz.Actor, partyID uuid.UUID) (PartyView, error) {
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyRead, adminSurface); err != nil {
		return PartyView{}, err
	}
	return s.reload(ctx, partyID, true)
}

// AdminUploadDocument implements Service (T-234): authz (403) → body →
// the party (404; inactive: 409) → the file: a party_document upload of
// the caller (422 otherwise) → in one transaction: the document row,
// files.Attach, audit. It serves the parties no org surface reaches (a
// foundation before its payee row, Koro's own party).
func (s *service) AdminUploadDocument(ctx context.Context, a authz.Actor, partyID uuid.UUID, in DocumentInput) (Document, error) {
	if err := authz.Can(ctx, a, authz.PartiesDocumentUpload, adminSurface); err != nil {
		return Document{}, err
	}
	if !in.Kind.Valid() {
		return Document{}, fieldErr("body.kind", "is not a document kind")
	}
	if s.d.Files == nil {
		return Document{}, ErrUnavailable
	}
	p, err := s.d.Store.GetParty(ctx, partyID)
	if err != nil {
		return Document{}, err
	}
	if p.Status == StatusInactive {
		return Document{}, ErrInvalidState
	}
	if err := s.checkOwnUpload(ctx, a, in.FileID, "body.file_id"); err != nil {
		return Document{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op        string
		Party, By uuid.UUID
		Kind      DocumentKind
		File      uuid.UUID
	}{"admin_upload_document", partyID, a.UserID, in.Kind, in.FileID})
	if err != nil {
		return Document{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.LockParty(ctx, partyID)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status == StatusInactive {
			return uuid.Nil(), ErrInvalidState
		}
		d, err := s.attachDocument(ctx, a, cur, in)
		if err != nil {
			return uuid.Nil(), err
		}
		return d.ID, s.d.Audit.Log(ctx, documentEvent(a, authz.PartiesDocumentUpload, nil, cur, d,
			map[string]any{"surface": "admin"}))
	})
	if err != nil {
		return Document{}, err
	}
	return s.d.Store.GetDocument(ctx, id, false)
}

// attachDocument writes a document row of p uploaded by a and attaches its
// file, in the transaction carried by ctx.
func (s *service) attachDocument(ctx context.Context, a authz.Actor, p LegalParty, in DocumentInput) (Document, error) {
	d, err := s.d.Store.InsertDocument(ctx, Document{
		ID: uuid.NewV7(), LegalPartyID: p.ID, Kind: in.Kind, FileID: in.FileID, UploadedBy: a.UserID,
	})
	if err != nil {
		return Document{}, err
	}
	owner := files.Owner{Module: documentModule, EntityID: d.ID, Purpose: files.PurposePartyDocument}
	if err := s.d.Files.Attach(ctx, a, in.FileID, owner); err != nil {
		return Document{}, err
	}
	return d, nil
}

// checkOwnUpload accepts a party_document upload of the caller (the admin
// surface has no org to accept uploads for).
func (s *service) checkOwnUpload(ctx context.Context, a authz.Actor, fileID uuid.UUID, loc string) error {
	bad := fieldErr(loc, "must be a party_document upload of the caller")
	f, err := s.d.Files.Get(ctx, fileID)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return bad
	case err != nil:
		return err
	}
	if f.Purpose != files.PurposePartyDocument || f.UploadedBy == nil || *f.UploadedBy != a.UserID {
		return bad
	}
	return nil
}

// AdminDownloadDocument implements Service (T-234): authz (403) → the
// document (404) → a 5-minute URL of a clean file (409 files.not_clean) →
// audit (every download, before the URL leaves).
func (s *service) AdminDownloadDocument(ctx context.Context, a authz.Actor, documentID uuid.UUID) (DocumentLink, error) {
	if err := authz.Can(ctx, a, authz.PartiesDocumentRead, adminSurface); err != nil {
		return DocumentLink{}, err
	}
	d, err := s.d.Store.GetDocument(ctx, documentID, false)
	if err != nil {
		return DocumentLink{}, err
	}
	p, err := s.d.Store.GetParty(ctx, d.LegalPartyID)
	if err != nil {
		return DocumentLink{}, err
	}
	if s.d.Files == nil {
		return DocumentLink{}, ErrUnavailable
	}
	dl, err := s.d.Files.PresignGet(ctx, d.FileID, docTTL)
	if err != nil {
		return DocumentLink{}, err
	}
	meta := map[string]any{"operation": "download", "surface": "admin", "ttl_seconds": int(docTTL.Seconds())}
	if err := s.d.Audit.Log(ctx, documentEvent(a, authz.PartiesDocumentRead, nil, p, d, meta)); err != nil {
		return DocumentLink{}, err
	}
	return DocumentLink{URL: dl.URL, ExpiresAt: dl.ExpiresAt}, nil
}

// AdminCorrectOwnership implements Service (06 H-60): a party holds a tax
// ID it does not own (another company registered the real owner's VKN
// first, and the owner gets 409 parties.tax_id_taken). A platform admin
// retires it with the evidence: authz on the admin surface (403 for
// non-staff) and on the correction (platform admins only: finance 403) →
// body (reason 10–500 characters, the evidence: a party_document upload of
// the caller) → in one transaction: the party, locked (404; the platform
// party or a party without a tax ID: 422; inactive: 409) → inactive
// (verification cleared; out of the tax ID uniqueness, so the owner can
// register) → the evidence kept as a document of the party (kind other) →
// audit (parties.legal_party.verify, operation ownership_correction, the
// reason). Orders keep their buyer snapshot.
func (s *service) AdminCorrectOwnership(ctx context.Context, a authz.Actor, partyID uuid.UUID, in OwnershipCorrection) (PartyView, error) {
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyVerify, adminSurface); err != nil {
		return PartyView{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyVerify, ownershipResource(partyID)); err != nil {
		return PartyView{}, err
	}
	reason, err := cleanReason("body.reason", in.Reason, minReason)
	if err != nil {
		return PartyView{}, err
	}
	if s.d.Files == nil {
		return PartyView{}, ErrUnavailable
	}
	if err := s.checkOwnUpload(ctx, a, in.EvidenceFileID, "body.evidence_file_id"); err != nil {
		return PartyView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Party  uuid.UUID
		Reason string
		File   uuid.UUID
	}{"ownership_correction", partyID, reason, in.EvidenceFileID})
	if err != nil {
		return PartyView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.LockParty(ctx, partyID)
		if err != nil {
			return uuid.Nil(), err
		}
		switch {
		case cur.Kind == KindPlatform:
			return uuid.Nil(), fieldErr("path.party_id", "the platform party is not corrected (it has no competing owner)")
		case cur.TaxNo == nil && !cur.HasTCKN():
			return uuid.Nil(), fieldErr("path.party_id", "the party holds no tax ID: retire it with adminUpdateLegalParty")
		case cur.Status == StatusInactive:
			return uuid.Nil(), ErrInvalidState
		}
		next := cur
		next.Status, next.VerifiedAt, next.VerifiedBy = StatusInactive, nil, nil
		stored, err := s.d.Store.UpdateParty(ctx, next)
		if err != nil {
			return uuid.Nil(), err
		}
		d, err := s.attachDocument(ctx, a, stored, DocumentInput{Kind: DocOther, FileID: in.EvidenceFileID})
		if err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{"operation": "ownership_correction", "from": string(cur.Status), "to": string(stored.Status),
			"evidence_document_id": d.ID.String()}
		return stored.ID, s.d.Audit.Log(ctx, partyEvent(a, authz.PartiesLegalPartyVerify, nil, stored, reason, meta))
	})
	if err != nil {
		return PartyView{}, err
	}
	return s.reload(ctx, partyID, true)
}
