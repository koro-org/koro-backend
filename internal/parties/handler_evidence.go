package parties

import (
	"context"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// replaceNote is the common part of the replace operations' descriptions.
const replaceNote = " Only when the file was rejected after its upload (found infected or not processable: its state is " +
	"rejected; 409 files.not_rejected otherwise); 409 files.rejected, files.not_clean or files.already_attached for the " +
	"new file. The rejected file stays on record (audit operation replace_rejected_file with both file IDs). " +
	"Idempotency-Key optional."

// registerEvidence adds the replace operations of a rejected document file
// or bank account evidence (BE-154b, T-397): the cell of the first upload,
// no new action.
func registerEvidence(api huma.API, h handler) {
	op.Register(api, "parties", huma.Operation{
		OperationID: "replaceLegalPartyDocumentFile", Method: http.MethodPost,
		Path:    "/orgs/{org_id}/legal-parties/{party_id}/documents/{document_id}/replace-file",
		Summary: "Upload a legal party document's file again",
		Description: "The people and checks of uploadLegalPartyDocument; file_id is a party_document upload of the caller or " +
			"of this organization (422 otherwise). The caller becomes the document's uploader and a verification is reset " +
			"(another finance admin verifies the new file; 06 H-60). A document of another party is 404." + replaceNote,
	}, op.Meta{Action: string(authz.PartiesDocumentUpload), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional},
		h.replaceDocumentFile)

	op.Register(api, "parties", huma.Operation{
		OperationID: "adminReplaceLegalPartyDocumentFile", Method: http.MethodPost,
		Path:    "/admin/legal-party-documents/{document_id}/replace-file",
		Summary: "Upload a legal party document's file again (finance and platform admins)",
		Description: "The people and checks of adminUploadLegalPartyDocument; file_id is a party_document upload of the caller " +
			"(422 otherwise). The caller becomes the document's uploader and a verification is reset." + replaceNote,
	}, op.Meta{Action: string(authz.PartiesDocumentUpload), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.adminReplaceDocumentFile)

	op.Register(api, "parties", huma.Operation{
		OperationID: "replacePayeeBankAccountEvidence", Method: http.MethodPost,
		Path:    "/orgs/{org_id}/payees/{payee_id}/bank-accounts/{bank_account_id}/replace-evidence",
		Summary: "Upload a bank account's evidence again",
		Description: "Requires re-authentication; the people of submitPayeeBankAccount. Only while the account waits for an " +
			"eye (409 parties.invalid_state; an approved account is submitted again), and an admin submission only by " +
			"finance or platform admins (409). evidence_file_id is a bank_account_evidence upload of this organization or " +
			"of the caller (422). An org submission waiting for the second eye goes back to pending_verification; the " +
			"IBAN, the holder and the submitter do not change, and whoever uploaded the evidence may not verify or approve " +
			"the account (403 authz.four_eyes_violation). An account of another payee is 404." + replaceNote,
	}, op.Meta{Action: string(authz.PartiesBankAccountSubmit), Surface: op.SurfaceOrg, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.replaceAccountEvidence)
}

// documentOut maps a document with the state of its file (read after the
// use case).
func (h handler) documentOut(ctx context.Context, d Document, staff bool) (*documentOutput, error) {
	st, err := h.svc.DocumentFileStates(ctx, &d.FileID)
	if err != nil {
		return nil, err
	}
	return &documentOutput{Body: documentDTO(d, st[d.FileID], staff)}, nil
}

// fileBody is the body of the document replacements.
type fileBody struct {
	FileID string `json:"file_id" format:"uuid" doc:"The new file: a party_document upload (createUpload, completeUpload)"`
}

// newFile parses the new file's ID (422 for a malformed one).
func newFile(loc, raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil(), problem.Field(loc, "must be a UUID")
	}
	return id, nil
}

type replaceDocumentFileInput struct {
	idempotency.OptionalHeader
	OrgID      string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	PartyID    string `path:"party_id" doc:"Legal party ID (an unknown, malformed or other organization's ID is 404)"`
	DocumentID string `path:"document_id" doc:"Document ID (an unknown, malformed or other party's ID is 404)"`
	Body       fileBody
}

func (h handler) replaceDocumentFile(ctx context.Context, in *replaceDocumentFileInput) (*documentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	file, err := newFile("body.file_id", in.Body.FileID)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ReplaceDocumentFile(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.PartyID), httpx.ParseID(in.DocumentID), file)
	if err != nil {
		return nil, err
	}
	return &documentOutput{Body: documentDTO(v.Document, v.FileState, fullTaxIDs(a))}, nil
}

type adminReplaceDocumentFileInput struct {
	idempotency.OptionalHeader
	DocumentID string `path:"document_id" doc:"Document ID (an unknown or malformed ID is 404)"`
	Body       fileBody
}

func (h handler) adminReplaceDocumentFile(ctx context.Context, in *adminReplaceDocumentFileInput) (*documentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	file, err := newFile("body.file_id", in.Body.FileID)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminReplaceDocumentFile(ctx, a, httpx.ParseID(in.DocumentID), file)
	if err != nil {
		return nil, err
	}
	return &documentOutput{Body: documentDTO(v.Document, v.FileState, true)}, nil
}

// replaceAccountEvidenceInput names its path parameters itself: huma reads
// the fields of an embedded struct only when its type is exported, so the
// unexported orgPayeePath would drop org_id and payee_id from the contract
// and leave them empty (TestOpenAPI_PathParametersMatchTheTemplate).
type replaceAccountEvidenceInput struct {
	idempotency.OptionalHeader
	OrgID string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	PayeePath
	BankAccountID string `path:"bank_account_id" doc:"Bank account ID (an unknown, malformed or other payee's ID is 404)"`
	Body          struct {
		EvidenceFileID string             `json:"evidence_file_id" format:"uuid" doc:"The new evidence: a bank_account_evidence upload"`
		Reauth         *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) replaceAccountEvidence(ctx context.Context, in *replaceAccountEvidenceInput) (*accountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	file, err := newFile("body.evidence_file_id", in.Body.EvidenceFileID)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ReplaceBankAccountEvidence(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.PayeeID),
		httpx.ParseID(in.BankAccountID), file, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &accountOutput{CacheControl: "no-store", Body: accountDTO(v)}, nil
}
