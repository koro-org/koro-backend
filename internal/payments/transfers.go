package payments

import (
	"context"
	"log/slog"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orders"
	"koro/internal/platform/civil"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// Limits of a recorded transfer (000105 CHECKs).
const (
	maxSenderName    = 300
	maxDescription   = 500
	maxBankReference = 100
	maxIgnoreReason  = 500
)

// transferDraft is a validated transfer before it is stored.
type transferDraft struct {
	ValueDate     civil.Date
	Amount        int64
	SenderName    string
	SenderIBAN    string
	Description   string
	BankReference string
}

// cleanDraft validates the bank data of a transfer (loc prefixes the field
// locations: "body." for the API, "" for a CSV row). The value date may not
// be in the future (Europe/Istanbul).
func (s *service) cleanDraft(loc string, in transferDraft) (transferDraft, error) {
	out := transferDraft{ValueDate: in.ValueDate, Amount: in.Amount}
	switch {
	case !in.ValueDate.IsValid():
		return transferDraft{}, fieldErr(loc+"value_date", "must be a date (YYYY-MM-DD)")
	case in.ValueDate.After(civil.Today(s.d.Clock.Now())):
		return transferDraft{}, fieldErr(loc+"value_date", "must not be in the future")
	case in.Amount <= 0:
		return transferDraft{}, fieldErr(loc+"amount", "must be a positive amount in kuruş")
	}
	var err error
	if out.Description, err = cleanText(loc+"description", in.Description, 1, maxDescription); err != nil {
		return transferDraft{}, err
	}
	if out.BankReference, err = cleanText(loc+"bank_reference", in.BankReference, 1, maxBankReference); err != nil {
		return transferDraft{}, err
	}
	if out.SenderName, err = cleanText(loc+"sender_name", in.SenderName, 0, maxSenderName); err != nil {
		return transferDraft{}, err
	}
	if in.SenderIBAN != "" {
		if out.SenderIBAN, err = cleanIBAN(loc+"sender_iban", in.SenderIBAN); err != nil {
			return transferDraft{}, err
		}
	}
	return out, nil
}

// newTransfer builds the unmatched row of a draft; the reference code of
// its description (if any) is stored for the suggestions (04 §C.4).
func (s *service) newTransfer(a authz.Actor, accountID uuid.UUID, d transferDraft, method EntryMethod) IncomingTransfer {
	now := s.now()
	t := IncomingTransfer{
		ID: uuid.NewV7(), CollectionAccountID: accountID, ValueDate: d.ValueDate, Amount: d.Amount, Currency: Currency,
		SenderName: optional(d.SenderName), SenderIBAN: optional(d.SenderIBAN), Description: d.Description,
		BankReference: d.BankReference, Status: TransferUnmatched, EntryMethod: method, EnteredBy: a.UserID,
		CreatedAt: now, UpdatedAt: now,
	}
	if ref, ok := orders.ExtractReference(d.Description); ok {
		t.ExtractedOrderNo = &ref
	}
	return t
}

// recordingAccount returns the account a transfer is recorded on: an
// approved one (active or retired: money may still arrive at a retired
// IBAN); a pending, rejected or unknown account is 422.
func (s *service) recordingAccount(ctx context.Context, loc string, id uuid.UUID) (CollectionAccount, error) {
	c, err := s.d.Store.GetAccount(ctx, id, false)
	switch {
	case isNotFound(err):
		return CollectionAccount{}, fieldErr(loc, "must be an approved collection account")
	case err != nil:
		return CollectionAccount{}, err
	case c.Status != AccountActive && c.Status != AccountRetired:
		return CollectionAccount{}, fieldErr(loc, "must be an approved collection account")
	}
	return c, nil
}

// CreateTransfer implements Service (fin): authz (403) → body (422) → the
// account (approved; 422) → the receipt (a transfer_receipt upload; 422) →
// in one transaction: the transfer (unmatched; 409
// payments.duplicate_bank_reference when the account has the reference),
// the receipt attached → audit.
func (s *service) CreateTransfer(ctx context.Context, a authz.Actor, in TransferInput) (IncomingTransfer, error) {
	if err := authz.Can(ctx, a, authz.PaymentsTransferCreate, adminSurface); err != nil {
		return IncomingTransfer{}, err
	}
	d, err := s.cleanDraft("body.", transferDraft{ValueDate: in.ValueDate, Amount: in.Amount, SenderName: in.SenderName,
		SenderIBAN: in.SenderIBAN, Description: in.Description, BankReference: in.BankReference})
	if err != nil {
		return IncomingTransfer{}, err
	}
	if _, err := s.recordingAccount(ctx, "body.collection_account_id", in.CollectionAccountID); err != nil {
		return IncomingTransfer{}, err
	}
	if in.ReceiptFileID != nil {
		if err := s.checkFile(ctx, "body.receipt_file_id", *in.ReceiptFileID, files.PurposeTransferReceipt); err != nil {
			return IncomingTransfer{}, err
		}
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Account uuid.UUID
		D       transferDraft
		Receipt *uuid.UUID
	}{"create_transfer", in.CollectionAccountID, d, in.ReceiptFileID})
	if err != nil {
		return IncomingTransfer{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		t := s.newTransfer(a, in.CollectionAccountID, d, EntryManual)
		t.ReceiptFileID = in.ReceiptFileID
		stored, inserted, err := s.d.Store.InsertTransfer(ctx, t)
		switch {
		case err != nil:
			return uuid.Nil(), err
		case !inserted:
			return uuid.Nil(), ErrDuplicateBankReference
		}
		if in.ReceiptFileID != nil {
			if err := s.attach(ctx, a, *in.ReceiptFileID, stored.ID, files.PurposeTransferReceipt); err != nil {
				return uuid.Nil(), err
			}
		}
		meta := map[string]any{"entry_method": string(EntryManual), "has_reference": stored.ExtractedOrderNo != nil}
		return stored.ID, s.d.Audit.Log(ctx, transferEvent(a, authz.PaymentsTransferCreate, stored, "", meta))
	})
	if err != nil {
		return IncomingTransfer{}, err
	}
	return s.d.Store.GetTransfer(ctx, id, false)
}

// ListTransfers implements Service: authz (403) → the query (422) → a page,
// newest value date first.
func (s *service) ListTransfers(ctx context.Context, a authz.Actor, q TransferQuery) (TransferPage, error) {
	if err := authz.Can(ctx, a, authz.PaymentsTransferList, adminSurface); err != nil {
		return TransferPage{}, err
	}
	if q.invalid != nil {
		return TransferPage{}, q.invalid
	}
	f := q.Filter
	switch {
	case f.Status != "" && !f.Status.Valid():
		return TransferPage{}, fieldErr("query.status", "is not a transfer status")
	case f.ValueFrom != nil && f.ValueTo != nil && f.ValueTo.Before(*f.ValueFrom):
		return TransferPage{}, fieldErr("query.value_date_to", "must not be before value_date_from")
	}
	if f.Q != "" {
		v, err := cleanText("query.q", f.Q, 0, 100)
		if err != nil {
			return TransferPage{}, err
		}
		f.Q = v
		if ref, ok := orders.NormalizeReference(v); ok {
			f.QRef = ref
		}
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListTransfers(ctx, f, q.After, limit+1)
	if err != nil {
		return TransferPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := TransferPage{Items: rows}
	if more {
		last := rows[len(rows)-1]
		out.Next = &TransferCursor{ValueDate: last.ValueDate, ID: last.ID}
	}
	return out, nil
}

// GetTransfer implements Service: authz (403) → the transfer (404) → the
// suggestions of an unmatched transfer: the order of its reference code,
// scored by its amount against what the order waits for.
func (s *service) GetTransfer(ctx context.Context, a authz.Actor, id uuid.UUID) (TransferView, error) {
	if err := authz.Can(ctx, a, authz.PaymentsTransferRead, adminSurface); err != nil {
		return TransferView{}, err
	}
	t, err := s.d.Store.GetTransfer(ctx, id, false)
	if err != nil {
		return TransferView{}, err
	}
	v := TransferView{Transfer: t, Suggestions: []Suggestion{}}
	if t.Status != TransferUnmatched || t.ExtractedOrderNo == nil {
		return v, nil
	}
	o, found, err := s.d.Orders.FindByReference(ctx, *t.ExtractedOrderNo)
	if err != nil || !found {
		return v, err
	}
	v.Suggestions = append(v.Suggestions, Suggestion{OrderID: o.ID, OrderNo: o.OrderNo, AmountDue: o.DueAmount,
		OrderStatus: o.Status, Score: score(t.Amount, o)})
	return v, nil
}

// score rates how well amount pays o (Suggestion).
func score(amount int64, o orders.PayableOrder) float64 {
	switch {
	case o.Status != orders.StatusAwaitingPayment || o.DueAmount <= 0:
		return 0
	case amount == o.DueAmount:
		return 1
	case amount < o.DueAmount:
		return 0.7
	default:
		return 0.5
	}
}

// IgnoreTransfer implements Service (fin): authz (403) → the reason (422)
// → the transfer (404) → in one transaction: unmatched only (409) →
// ignored with its reason → audit (the reason is the audit reason).
func (s *service) IgnoreTransfer(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (IncomingTransfer, error) {
	if err := authz.Can(ctx, a, authz.PaymentsTransferIgnore, adminSurface); err != nil {
		return IncomingTransfer{}, err
	}
	why, err := cleanText("body.ignore_reason", reason, 1, maxIgnoreReason)
	if err != nil {
		return IncomingTransfer{}, err
	}
	if _, err := s.d.Store.GetTransfer(ctx, id, false); err != nil {
		return IncomingTransfer{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op, Reason string
		Transfer   uuid.UUID
	}{"ignore_transfer", why, id})
	if err != nil {
		return IncomingTransfer{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetTransfer(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != TransferUnmatched {
			return uuid.Nil(), stateErr(cur)
		}
		next := cur
		next.Status, next.IgnoreReason = TransferIgnored, &why
		stored, err := s.d.Store.UpdateTransfer(ctx, next, TransferUnmatched)
		if err != nil {
			return uuid.Nil(), err
		}
		s.logInfo(ctx, "incoming transfer ignored", slog.String("transfer_id", stored.ID.String()))
		return stored.ID, s.d.Audit.Log(ctx, transferEvent(a, authz.PaymentsTransferIgnore, stored, why, nil))
	})
	if err != nil {
		return IncomingTransfer{}, err
	}
	return s.d.Store.GetTransfer(ctx, id, false)
}

// stateErr is the 409 of a transfer that is not in the expected state: a
// matched one says so (04 §1.10 payments.transfer_already_matched).
func stateErr(t IncomingTransfer) error {
	if t.Status == TransferMatched {
		return ErrTransferAlreadyMatched
	}
	return ErrInvalidState
}
