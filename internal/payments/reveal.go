package payments

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/parties"
	"koro/internal/platform/iban"
	"koro/internal/platform/problem"
)

// BE-129: no list or detail response of payments carries a full IBAN (data
// minimisation; payment security). Every one shows the masked form; a
// finance or platform admin reveals one number at a time, with the access
// reason, audited like a break-glass read (parties.RevealIBAN).

// TransferIBAN names an IBAN of an incoming transfer that
// adminRevealIncomingTransferIban reveals.
type TransferIBAN string

// The IBANs of a transfer.
const (
	// TransferIBANSender is sender_iban_masked: the account the money came
	// from (a return's default target).
	TransferIBANSender TransferIBAN = "sender"
	// TransferIBANReturnTarget is return.target_iban_masked: where the
	// return EFT goes once approved.
	TransferIBANReturnTarget TransferIBAN = "return_target"
)

// TransferIBANs lists the IBANs of a transfer (the field enum).
func TransferIBANs() []TransferIBAN {
	return []TransferIBAN{TransferIBANSender, TransferIBANReturnTarget}
}

// receiptTTL bounds the download URL of a transfer's bank receipt (04
// §1.14: at most 5 minutes).
const receiptTTL = 5 * time.Minute

// masked is the masked form of an optional IBAN ("" when unknown).
func masked(number *string) string {
	if number == nil {
		return ""
	}
	return iban.Mask(*number)
}

// reveal is the end of every payments reveal: the access reason (422)
// and the audit record of payments.iban.reveal, then the number; an IBAN
// the resource does not hold is 404.
func (s *service) reveal(ctx context.Context, a authz.Actor, resourceType string, id uuid.UUID, field string, number *string, reason string) (string, error) {
	if number == nil {
		return "", ErrNotFound
	}
	return parties.RevealIBAN(ctx, s.d.Audit, a, parties.Revealed{Action: authz.PaymentsIbanReveal, ResourceType: resourceType,
		ResourceID: id, Field: field, IBAN: *number}, reason)
}

// RevealCollectionAccountIBAN implements Service (BE-129): authz (403
// before reading) → the account (404) → the reveal (422, audited). The
// second eye checks the number against Koro's bank before approving it.
func (s *service) RevealCollectionAccountIBAN(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (string, error) {
	if err := authz.Can(ctx, a, authz.PaymentsIbanReveal, adminSurface); err != nil {
		return "", err
	}
	c, err := s.d.Store.GetAccount(ctx, id, false)
	if err != nil {
		return "", err
	}
	return s.reveal(ctx, a, authz.TypeCollectionAccount, c.ID, "iban", &c.IBAN, reason)
}

// RevealTransferIBAN implements Service (BE-129): authz (403) → the field
// (422) → the transfer (404) → the IBAN it holds (404: none recorded, no
// return requested) → the reveal (422, audited).
func (s *service) RevealTransferIBAN(ctx context.Context, a authz.Actor, id uuid.UUID, which TransferIBAN, reason string) (string, error) {
	if err := authz.Can(ctx, a, authz.PaymentsIbanReveal, adminSurface); err != nil {
		return "", err
	}
	if which != TransferIBANSender && which != TransferIBANReturnTarget {
		return "", problem.Field("query.field", "must be sender or return_target")
	}
	t, err := s.d.Store.GetTransfer(ctx, id, false)
	if err != nil {
		return "", err
	}
	if which == TransferIBANSender {
		return s.reveal(ctx, a, authz.TypeIncomingTransfer, t.ID, "sender_iban", t.SenderIBAN, reason)
	}
	return s.reveal(ctx, a, authz.TypeIncomingTransfer, t.ID, "return.target_iban", t.ReturnTargetIBAN, reason)
}

// RevealRefundIBAN implements Service (BE-129): authz (403) → the refund
// (404) → its target IBAN (404 without one) → the reveal (422, audited).
// The finance admin types the refund EFT with it.
func (s *service) RevealRefundIBAN(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (string, error) {
	if err := authz.Can(ctx, a, authz.PaymentsIbanReveal, adminSurface); err != nil {
		return "", err
	}
	r, err := s.d.Store.GetRefund(ctx, id, false)
	if err != nil {
		return "", err
	}
	return s.reveal(ctx, a, authz.TypeRefund, r.ID, "target_iban", r.TargetIBAN, reason)
}

// TransferReceipt implements Service (BE-129): authz
// (payments.transfer.read, 403) → the transfer (404) → its bank receipt
// (404 without one) → a URL of the clean file valid for 5 minutes (409
// files.not_clean) → the audit record of the download (IDs only) before
// the URL leaves.
func (s *service) TransferReceipt(ctx context.Context, a authz.Actor, id uuid.UUID) (files.Download, error) {
	if err := authz.Can(ctx, a, authz.PaymentsTransferRead, adminSurface); err != nil {
		return files.Download{}, err
	}
	t, err := s.d.Store.GetTransfer(ctx, id, false)
	if err != nil {
		return files.Download{}, err
	}
	if t.ReceiptFileID == nil {
		return files.Download{}, ErrNotFound
	}
	if s.d.Files == nil {
		return files.Download{}, ErrUnavailable
	}
	d, err := s.d.Files.PresignGet(ctx, *t.ReceiptFileID, receiptTTL)
	if err != nil {
		return files.Download{}, err
	}
	meta := map[string]any{"operation": "download_receipt", "receipt_file_id": t.ReceiptFileID.String(),
		"ttl_seconds": int(receiptTTL.Seconds())}
	if err := s.d.Audit.Log(ctx, transferEvent(a, authz.PaymentsTransferRead, t, "", meta)); err != nil {
		return files.Download{}, err
	}
	return d, nil
}
