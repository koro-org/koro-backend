package payments

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/iban"
	"koro/internal/platform/textx"
)

// Names of the rows as files owners and event aggregates.
const (
	moduleName         = "payments"
	aggregateAccount   = "collection_account"
	aggregateTransfer  = "incoming_transfer"
	aggregatePayment   = "payment"
	aggregateUnmatched = "incoming_transfer_digest"
)

// accountResource is the authz view of a collection account for the
// second eye (03 §3.5 collection_account: status, created_by).
func accountResource(c CollectionAccount) authz.Resource {
	return authz.Resource{Type: authz.TypeCollectionAccount, ID: c.ID, Attrs: map[string]string{
		authz.AttrStatus: string(c.Status), authz.AttrCreatedBy: c.CreatedBy.String(),
	}}
}

// transferResource is the authz view of a transfer for the return steps
// (06 H-18): requested_by is the requester only for the second eye's
// decision; the request and the receipt have no earlier eye to avoid.
func transferResource(t IncomingTransfer, secondEye bool) authz.Resource {
	requested := ""
	if secondEye && t.ReturnRequestedBy != nil {
		requested = t.ReturnRequestedBy.String()
	}
	return authz.Resource{Type: authz.TypeIncomingTransfer, ID: t.ID, Attrs: map[string]string{
		authz.AttrStatus: string(t.Status), authz.AttrRequestedBy: requested,
	}}
}

// unreadTransfer is the "403 before reading" view of a return step: the
// transfer is not loaded yet, so it carries no earlier eye.
var unreadTransfer = authz.Resource{Type: authz.TypeIncomingTransfer, Attrs: map[string]string{authz.AttrRequestedBy: ""}}

// accountEvent is the audit record of a collection account action. The
// IBAN is recorded masked only.
func accountEvent(a authz.Actor, act authz.Action, c CollectionAccount, meta map[string]any) audit.Event {
	id := c.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["iban_masked"] = c.MaskedIBAN()
	meta["status"] = string(c.Status)
	meta["is_default"] = c.IsDefault
	return audit.Event{
		Action: string(act), Outcome: audit.Success, ResourceType: authz.TypeCollectionAccount, ResourceID: &id, Metadata: meta,
	}.By(a)
}

// transferEvent is the audit record of a transfer action: IDs, amounts and
// states, never the sender's name or IBAN (a natural person's, maybe).
func transferEvent(a authz.Actor, act authz.Action, t IncomingTransfer, reason string, meta map[string]any) audit.Event {
	id := t.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["status"] = string(t.Status)
	meta["amount"] = t.Amount
	meta["collection_account_id"] = t.CollectionAccountID.String()
	return audit.Event{
		Action: string(act), Outcome: audit.Success, ResourceType: authz.TypeIncomingTransfer, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// emit writes an O event in the transaction carried by ctx.
func (s *service) emit(ctx context.Context, aggregate string, id uuid.UUID, p domainevent.Payload) error {
	env, err := domainevent.New(p, aggregate, id)
	if err != nil {
		return err
	}
	return s.d.Outbox.Add(ctx, env)
}

// cleanText normalises a single-line text (05 §9.5) and checks its length
// in characters; min 0 makes it optional.
func cleanText(loc, s string, minLen, maxLen int) (string, error) {
	v, err := textx.NormalizeText(s)
	if err != nil {
		return "", fieldErr(loc, "contains characters that are not allowed")
	}
	n := utf8.RuneCountInString(v)
	switch {
	case n == 0 && minLen > 0:
		return "", fieldErr(loc, "is required")
	case n > 0 && n < minLen:
		return "", fieldErr(loc, fmt.Sprintf("must be at least %d characters", minLen))
	case n > maxLen:
		return "", fieldErr(loc, fmt.Sprintf("must be at most %d characters", maxLen))
	}
	return v, nil
}

// cleanIBAN normalises an IBAN; a malformed one is 422 parties.iban_invalid
// with the field's location.
func cleanIBAN(loc, s string) (string, error) {
	n, err := iban.Parse(s)
	if err != nil {
		return "", ErrIBANInvalid.WithFields(fieldErrOf(loc, "must be a Turkish IBAN (TR + 24 digits)"))
	}
	return n, nil
}

// optional returns nil for an empty string.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// checkFile accepts an active upload of the expected purpose that no other
// record holds yet (Attach checks again in the transaction).
func (s *service) checkFile(ctx context.Context, loc string, id uuid.UUID, purpose files.Purpose) error {
	if s.d.Files == nil {
		return ErrUnavailable
	}
	bad := fieldErr(loc, fmt.Sprintf("must be a %s upload", purpose))
	f, err := s.d.Files.Get(ctx, id)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return bad
	case err != nil:
		return err
	case f.Purpose != purpose:
		return bad
	}
	return nil
}

// attach binds a file to its payments row in the transaction carried by ctx.
func (s *service) attach(ctx context.Context, a authz.Actor, fileID, owner uuid.UUID, purpose files.Purpose) error {
	if s.d.Files == nil {
		return ErrUnavailable
	}
	return s.d.Files.Attach(ctx, a, fileID, files.Owner{Module: moduleName, EntityID: owner, Purpose: purpose})
}

// isNotFound reports a missing row.
func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
