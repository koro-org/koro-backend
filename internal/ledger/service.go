package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/money"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

type service struct{ d Deps }

var _ Service = (*service)(nil)

// actorOf returns the user of ctx's actor (HTTP paths), or nil (workers):
// ledger_transactions.created_by.
func actorOf(ctx context.Context) *uuid.UUID {
	if a := authz.ActorOrAnonymous(ctx); a.IsUser() && a.UserID != uuid.Nil() {
		id := a.UserID
		return &id
	}
	return nil
}

// Post implements Poster.
func (s *service) Post(ctx context.Context, t Txn) (uuid.UUID, error) {
	if err := t.validate(); err != nil {
		return uuid.Nil(), err
	}
	var id uuid.UUID
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		id, err = s.post(ctx, t)
		return err
	})
	if err != nil {
		// Own transaction: a deferred ledger_txn_balanced failure at COMMIT
		// is ledger.unbalanced, not validation_failed (T-44).
		return uuid.Nil(), TranslateError(err)
	}
	return id, nil
}

// post writes a validated t in ctx's transaction.
//
// Order matters for the race-free checks: the payee locks come first, so a
// concurrent duplicate of a debit is seen as a replay (its first copy has
// committed by the time the lock is ours) instead of failing the balance
// check; the balance is read after the lock, so concurrent debits of one
// payee see each other.
func (s *service) post(ctx context.Context, t Txn) (uuid.UUID, error) {
	debits := payeeDebits(t)
	if len(debits) > 0 {
		ids := make([]uuid.UUID, len(debits))
		for i, d := range debits {
			ids[i] = d.PayeeID
		}
		if err := s.d.Store.LockPayees(ctx, ids); err != nil {
			return uuid.Nil(), err
		}
	}
	if id, done, err := s.replay(ctx, t); done || err != nil {
		return id, err
	}
	for _, d := range debits {
		bal, err := s.d.Store.PayeeBalance(ctx, d.PayeeID)
		if err != nil {
			return uuid.Nil(), err
		}
		if after, err := money.Sub(bal, d.Amount); err != nil || after < 0 {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "ledger post rejected: payee balance would be negative",
				slog.String("kind", string(t.Kind)), slog.String("ref_type", string(t.RefType)),
				slog.String("ref_id", t.RefID.String()), slog.String("payee_id", d.PayeeID.String()),
				slog.Int64("balance", bal), slog.Int64("debit", d.Amount))
			return uuid.Nil(), ErrNegativeBalance
		}
	}

	now := s.d.Clock.Now().UTC()
	h := Transaction{
		ID: uuid.NewV7(), Kind: t.Kind, RefType: t.RefType, RefID: t.RefID, OccurredAt: now,
		CreatedBy: actorOf(ctx), CreatedAt: now,
	}
	if !t.OccurredAt.IsZero() {
		h.OccurredAt = t.OccurredAt.UTC()
	}
	if t.Memo != "" {
		memo := t.Memo
		h.Memo = &memo
	}
	rows := make([]EntryRow, len(t.Entries))
	for i, e := range t.Entries {
		rows[i] = EntryRow{
			ID: uuid.NewV7(), TransactionID: h.ID, Account: e.Account, PayeeID: e.PayeeID, OrderLineID: e.OrderLineID,
			Amount: e.Amount, Currency: Currency, CreatedAt: now,
		}
	}
	inserted, err := s.d.Store.InsertTxn(ctx, h, rows)
	if err != nil {
		return uuid.Nil(), err
	}
	if !inserted {
		// A concurrent post of the same event committed between the replay
		// check and the insert.
		id, done, err := s.replay(ctx, t)
		if err == nil && !done {
			err = fmt.Errorf("ledger: %s/%s/%s neither inserted nor found", t.Kind, t.RefType, t.RefID)
		}
		return id, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "ledger transaction posted",
		slog.String("transaction_id", h.ID.String()), slog.String("kind", string(t.Kind)),
		slog.String("ref_type", string(t.RefType)), slog.String("ref_id", t.RefID.String()),
		slog.Int("entries", len(rows)), slog.Int64("total", t.Total()))
	return h.ID, nil
}

// replay looks up an earlier post of t's business event: done=true with its
// ID when the entries are the same, ErrRefConflict when they differ.
func (s *service) replay(ctx context.Context, t Txn) (uuid.UUID, bool, error) {
	p, err := s.d.Store.ByRef(ctx, t.Kind, t.RefType, t.RefID)
	switch {
	case errors.Is(err, ErrNotFound):
		return uuid.Nil(), false, nil
	case err != nil:
		return uuid.Nil(), false, err
	}
	stored := make([]Entry, len(p.Entries))
	for i, r := range p.Entries {
		stored[i] = r.Entry()
	}
	if !sameEntries(stored, t.Entries) {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "ledger event posted again with other entries",
			slog.String("transaction_id", p.ID.String()), slog.String("kind", string(t.Kind)),
			slog.String("ref_type", string(t.RefType)), slog.String("ref_id", t.RefID.String()))
		return uuid.Nil(), true, ErrRefConflict
	}
	return p.ID, true, nil
}

// Reverse implements Service.
func (s *service) Reverse(ctx context.Context, txnID uuid.UUID, memo string) (uuid.UUID, error) {
	if txnID == uuid.Nil() {
		return uuid.Nil(), invalid("transaction_id", "is required")
	}
	var id uuid.UUID
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		orig, err := s.d.Store.ByID(ctx, txnID)
		if err != nil {
			return err
		}
		t := Txn{Kind: KindAdjustment, RefType: RefManual, RefID: orig.ID, Memo: memo, Entries: make([]Entry, len(orig.Entries))}
		for i, r := range orig.Entries {
			e := r.Entry()
			e.Amount = -e.Amount // validate rejects math.MinInt64 (|x| has no int64 negation)
			t.Entries[i] = e
		}
		if err := t.validate(); err != nil {
			return err
		}
		id, err = s.post(ctx, t)
		return err
	})
	if err != nil {
		return uuid.Nil(), TranslateError(err)
	}
	return id, nil
}

// PayeeBalance implements Balances.
func (s *service) PayeeBalance(ctx context.Context, payeeID uuid.UUID) (int64, error) {
	if payeeID == uuid.Nil() {
		return 0, invalid("payee_id", "is required")
	}
	return s.d.Store.PayeeBalance(ctx, payeeID)
}

// adminSurface is the authz view of /admin/** (03 §3.2: 403 before reading).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// JournalQuery narrows adminListLedgerTransactions.
type JournalQuery struct {
	Filter JournalFilter
	Limit  int
	After  *JournalCursor // the last row of the previous page

	// invalid is a malformed query parameter or cursor found by the
	// handler. It is reported after authorisation: an actor without a
	// platform role gets 403 before any 422 (03 §7.2, steps 3 and 9).
	invalid error
}

// JournalPage is a page of the journal, newest occurred_at first.
type JournalPage struct {
	Items []Posted
	Next  *JournalCursor // nil on the last page
}

// ListTransactions implements Service.
func (s *service) ListTransactions(ctx context.Context, a authz.Actor, q JournalQuery) (JournalPage, error) {
	if err := authz.Can(ctx, a, authz.LedgerJournalList, adminSurface); err != nil {
		return JournalPage{}, err
	}
	if q.invalid != nil {
		return JournalPage{}, q.invalid
	}
	f := q.Filter
	switch {
	case f.Kind != "" && !f.Kind.Valid():
		return JournalPage{}, problem.Field("query.kind", "is not a known transaction kind")
	case f.RefType != "" && !f.RefType.Valid():
		return JournalPage{}, problem.Field("query.ref_type", "is not a known reference type")
	case f.From != nil && f.To != nil && !f.To.After(*f.From):
		return JournalPage{}, problem.Field("query.occurred_to", "must be after occurred_from")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.List(ctx, f, q.After, limit+1)
	if err != nil {
		return JournalPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := JournalPage{Items: rows}
	if more {
		last := cursorOf(rows[len(rows)-1])
		out.Next = &last
	}
	return out, nil
}
