package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
)

// NewGormStore returns the PostgreSQL store over ledger_transactions,
// ledger_entries, ledger_accounts and (read only, integrity check) payouts
// (02 §21).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// errNoTx is a programmer error: a ledger write outside a transaction would
// commit the header alone and fail the deferred balance check.
var errNoTx = errors.New("ledger: InsertTxn needs a transaction (db.WithTx)")

// payeeLockKey namespaces the advisory locks of payee balance checks.
const payeeLockKey = "ledger.payee:"

// InsertTxn implements Store. The header is inserted with ON CONFLICT DO
// NOTHING: a replay of the same business event inside a caller's
// transaction must not abort it with a unique violation (23505 would roll
// the whole payment back). Header and entries take created_at from now(),
// the transaction start, so the integrity check can spot entries written
// later (T-226). SET CONSTRAINTS … IMMEDIATE runs the deferred
// ledger_txn_balanced check now, over every ledger row of the running
// transaction (this one and anything inserted around Post), and DEFERRED
// restores the mode for later statements.
//
// cross-org: ledger rows have no org (finance data).
func (s gormStore) InsertTxn(ctx context.Context, h Transaction, entries []EntryRow) (bool, error) {
	if !db.InTx(ctx) {
		return false, errNoTx
	}
	conn := db.Conn(ctx, s.orm)
	var ids []struct{ ID uuid.UUID }
	err := conn.Raw(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at, memo, created_by)
		VALUES ( ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT ON CONSTRAINT ledger_transactions_ref_uq DO NOTHING
		RETURNING id`,
		h.ID, h.Kind, h.RefType, h.RefID, h.OccurredAt, h.Memo, h.CreatedBy).Scan(&ids).Error
	if err != nil {
		return false, fmt.Errorf("ledger: insert transaction: %w", constraints.Translate(err))
	}
	if len(ids) == 0 {
		return false, nil
	}
	var sql strings.Builder
	args := make([]any, 0, 6*len(entries))
	sql.WriteString(`INSERT INTO ledger_entries (id, transaction_id, account_code, payee_id, order_line_id, amount) VALUES `)
	for i, e := range entries {
		if i > 0 {
			sql.WriteString(", ")
		}
		sql.WriteString("( ?, ?, ?, ?, ?, ?)")
		args = append(args, e.ID, h.ID, string(e.Account), e.PayeeID, e.OrderLineID, e.Amount)
	}
	if err := conn.Exec(sql.String(), args...).Error; err != nil {
		return false, fmt.Errorf("ledger: insert entries: %w", constraints.Translate(err))
	}
	if err := conn.Exec(`SET CONSTRAINTS ledger_txn_balanced IMMEDIATE`).Error; err != nil {
		return false, fmt.Errorf("ledger: balance check: %w", constraints.Translate(err))
	}
	if err := conn.Exec(`SET CONSTRAINTS ledger_txn_balanced DEFERRED`).Error; err != nil {
		return false, fmt.Errorf("ledger: balance check: %w", err)
	}
	return true, nil
}

// ByRef implements Store.
//
// cross-org: ledger rows have no org (finance data).
func (s gormStore) ByRef(ctx context.Context, k Kind, r RefType, refID uuid.UUID) (Posted, error) {
	return s.one(ctx, "kind = ? AND ref_type = ? AND ref_id = ?", k, r, refID)
}

// ByID implements Store.
//
// cross-org: ledger rows have no org (finance data).
func (s gormStore) ByID(ctx context.Context, id uuid.UUID) (Posted, error) {
	return s.one(ctx, "id = ?", id)
}

// one loads a single transaction with its entries.
//
// cross-org: ledger rows have no org (finance data).
func (s gormStore) one(ctx context.Context, where string, args ...any) (Posted, error) {
	var h Transaction
	err := db.Conn(ctx, s.orm).Where(where, args...).Take(&h).Error
	switch {
	case db.IsNotFound(err):
		return Posted{}, ErrNotFound
	case err != nil:
		return Posted{}, fmt.Errorf("ledger: get transaction: %w", err)
	}
	out, err := s.withEntries(ctx, []Transaction{h})
	if err != nil {
		return Posted{}, err
	}
	return out[0], nil
}

// withEntries loads the entries of the headers. Headers and entries are
// written in one database transaction and never change, so a second
// statement sees every entry of a header the first one returned.
//
// cross-org: ledger rows have no org (finance data).
func (s gormStore) withEntries(ctx context.Context, hs []Transaction) ([]Posted, error) {
	out := make([]Posted, len(hs))
	if len(hs) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(hs))
	at := make(map[uuid.UUID]int, len(hs))
	for i, h := range hs {
		ids[i], at[h.ID] = h.ID, i
		out[i].Transaction = h
	}
	var rows []EntryRow
	if err := db.Conn(ctx, s.orm).Where("transaction_id IN ?", ids).Order("transaction_id, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ledger: get entries: %w", err)
	}
	for _, r := range rows {
		i := at[r.TransactionID]
		out[i].Entries = append(out[i].Entries, r)
	}
	return out, nil
}

// LockPayees implements Store with transaction-scoped advisory locks (one
// per payee, in the given order; the ledger does not lock another module's
// payees row).
//
// cross-org: payees are global IDs here; the lock guards ledger rows only.
func (s gormStore) LockPayees(ctx context.Context, payeeIDs []uuid.UUID) error {
	conn := db.Conn(ctx, s.orm)
	for _, id := range payeeIDs {
		if err := conn.Exec(`SELECT pg_advisory_xact_lock(hashtextextended( ?, 0))`, payeeLockKey+id.String()).Error; err != nil {
			return fmt.Errorf("ledger: lock payee: %w", db.Translate(err))
		}
	}
	return nil
}

// PayeeBalance implements Store: one statement, one snapshot. Per-payee
// accounts are liabilities (TestChart_PerPayeeAccountsAreLiabilities): a
// credit (negative amount) raises what Koro owes.
//
// cross-org: the payee is the scope; authorisation is the caller's.
func (s gormStore) PayeeBalance(ctx context.Context, payeeID uuid.UUID) (int64, error) {
	var bal int64
	err := db.Conn(ctx, s.orm).Raw(`SELECT coalesce(-sum(e.amount), 0)::bigint
		FROM ledger_entries e JOIN ledger_accounts a ON a.code = e.account_code
		WHERE a.per_payee AND e.payee_id = ?`, payeeID).Scan(&bal).Error
	if err != nil {
		return 0, fmt.Errorf("ledger: payee balance: %w", db.Translate(err))
	}
	return bal, nil
}

// List implements Store.
//
// cross-org: the journal is a platform view (ledger.journal.list).
func (s gormStore) List(ctx context.Context, f JournalFilter, after *JournalCursor, limit int) ([]Posted, error) {
	q := db.Conn(ctx, s.orm).Model(&Transaction{})
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.RefType != "" {
		q = q.Where("ref_type = ?", f.RefType)
	}
	if f.RefID != nil {
		q = q.Where("ref_id = ?", *f.RefID)
	}
	if f.From != nil {
		q = q.Where("occurred_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("occurred_at < ?", *f.To)
	}
	if after != nil {
		q = q.Where("(occurred_at, id) < ( ?, ?)", after.OccurredAt, after.ID)
	}
	var hs []Transaction
	if err := q.Order("occurred_at DESC, id DESC").Limit(limit).Find(&hs).Error; err != nil {
		return nil, fmt.Errorf("ledger: list: %w", err)
	}
	return s.withEntries(ctx, hs)
}

// RefundedShares implements Store (ledger_entries_line_idx).
//
// cross-org: one order line the caller resolved.
func (s gormStore) RefundedShares(ctx context.Context, lineID uuid.UUID) (LineShares, error) {
	var rows []struct {
		Account Account `gorm:"column:account_code"`
		Amount  int64   `gorm:"column:amount"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT e.account_code, sum(e.amount)::bigint AS amount
		FROM ledger_entries e JOIN ledger_transactions t ON t.id = e.transaction_id
		WHERE e.order_line_id = ? AND t.kind = 'refund_completed'
		GROUP BY e.account_code`, lineID).Scan(&rows).Error
	if err != nil {
		return LineShares{}, fmt.Errorf("ledger: refunded shares: %w", err)
	}
	var out LineShares
	for _, r := range rows {
		out.add(r.Account, r.Amount)
	}
	return out, nil
}

// LineAllocations implements Store (ledger_entries_line_idx).
//
// cross-org: order lines the caller resolved (the allocation check).
func (s gormStore) LineAllocations(ctx context.Context, lineIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	out := make(map[uuid.UUID]int64, len(lineIDs))
	if len(lineIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		LineID uuid.UUID `gorm:"column:order_line_id"`
		Amount int64     `gorm:"column:amount"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT e.order_line_id, sum(e.amount)::bigint AS amount
		FROM ledger_entries e JOIN ledger_transactions t ON t.id = e.transaction_id
		WHERE e.order_line_id IN ? AND e.account_code = 'buyer_advances' AND t.kind IN ('order_allocated', 'adjustment')
		GROUP BY e.order_line_id`, lineIDs).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("ledger: line allocations: %w", err)
	}
	for _, r := range rows {
		out[r.LineID] = r.Amount
	}
	return out, nil
}

// Unbalanced implements Store.
//
// cross-org: a platform job over the whole ledger.
func (s gormStore) Unbalanced(ctx context.Context, limit int) ([]UnbalancedTxn, error) {
	var rows []struct {
		TransactionID uuid.UUID `gorm:"column:transaction_id"`
		Entries       int       `gorm:"column:entries"`
		Total         *int64    `gorm:"column:total"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT t.id AS transaction_id, count(e.id) AS entries,
			CASE WHEN abs(coalesce(sum(e.amount), 0)) <= 9223372036854775807
			     THEN coalesce(sum(e.amount), 0)::bigint END AS total
		FROM ledger_transactions t LEFT JOIN ledger_entries e ON e.transaction_id = t.id
		GROUP BY t.id
		HAVING count(e.id) < 2 OR coalesce(sum(e.amount), 0) <> 0
		ORDER BY t.id LIMIT ?`, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("ledger: unbalanced: %w", err)
	}
	out := make([]UnbalancedTxn, len(rows))
	for i, r := range rows {
		out[i] = UnbalancedTxn{TransactionID: r.TransactionID, Entries: r.Entries, Sum: r.Total}
	}
	return out, nil
}

// NegativePayeeBalances implements Store.
//
// cross-org: a platform job over the whole ledger.
func (s gormStore) NegativePayeeBalances(ctx context.Context, limit int) ([]PayeeBalance, error) {
	var rows []struct {
		PayeeID uuid.UUID `gorm:"column:payee_id"`
		Balance int64     `gorm:"column:balance"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT e.payee_id, greatest(-sum(e.amount), -9223372036854775807)::bigint AS balance
		FROM ledger_entries e JOIN ledger_accounts a ON a.code = e.account_code
		WHERE a.per_payee AND e.payee_id IS NOT NULL
		GROUP BY e.payee_id
		HAVING sum(e.amount) > 0
		ORDER BY e.payee_id LIMIT ?`, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("ledger: negative balances: %w", err)
	}
	out := make([]PayeeBalance, len(rows))
	for i, r := range rows {
		out[i] = PayeeBalance{PayeeID: r.PayeeID, Balance: r.Balance}
	}
	return out, nil
}

// LateEntries implements Store.
//
// cross-org: a platform job over the whole ledger.
func (s gormStore) LateEntries(ctx context.Context, limit int) ([]LateEntry, error) {
	var rows []struct {
		TransactionID uuid.UUID `gorm:"column:transaction_id"`
		EntryID       uuid.UUID `gorm:"column:entry_id"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT e.transaction_id, e.id AS entry_id
		FROM ledger_entries e JOIN ledger_transactions t ON t.id = e.transaction_id
		WHERE e.created_at <> t.created_at
		ORDER BY e.id LIMIT ?`, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("ledger: late entries: %w", err)
	}
	out := make([]LateEntry, len(rows))
	for i, r := range rows {
		out[i] = LateEntry{TransactionID: r.TransactionID, EntryID: r.EntryID}
	}
	return out, nil
}

// PayoutMismatches implements Store. payouts belongs to the ledger module
// (02 §21.4; the payout flow, BE-044, writes it: PayoutStore).
//
// cross-org: a platform job over every payout.
func (s gormStore) PayoutMismatches(ctx context.Context, limit int) ([]PayoutMismatch, error) {
	var rows []struct {
		PayoutID      uuid.UUID  `gorm:"column:payout_id"`
		TransactionID *uuid.UUID `gorm:"column:transaction_id"`
		Problem       string     `gorm:"column:problem"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT * FROM (
		SELECT p.id AS payout_id, t.id AS transaction_id,
		       CASE WHEN t.id IS NULL THEN 'missing_transaction' ELSE 'entries_mismatch' END AS problem
		  FROM payouts p
		  LEFT JOIN ledger_transactions t ON t.kind = 'payout_paid' AND t.ref_type = 'payout' AND t.ref_id = p.id
		 WHERE p.status = 'paid'
		   AND (t.id IS NULL OR NOT (
		        ( SELECT count(*) FROM ledger_entries e WHERE e.transaction_id = t.id) = 2
		        AND EXISTS ( SELECT 1 FROM ledger_entries e WHERE e.transaction_id = t.id
		                     AND e.account_code = 'seller_payable' AND e.payee_id = p.payee_id AND e.amount = p.amount)
		        AND EXISTS ( SELECT 1 FROM ledger_entries e WHERE e.transaction_id = t.id
		                     AND e.account_code = 'koro_bank' AND e.amount = -p.amount)))
		UNION ALL
		SELECT t.ref_id, t.id, 'payout_not_paid'
		  FROM ledger_transactions t LEFT JOIN payouts p ON p.id = t.ref_id
		 WHERE t.kind = 'payout_paid' AND (p.id IS NULL OR p.status <> 'paid')
		) m ORDER BY payout_id LIMIT ?`, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("ledger: payout mismatches: %w", err)
	}
	out := make([]PayoutMismatch, len(rows))
	for i, r := range rows {
		out[i] = PayoutMismatch{PayoutID: r.PayoutID, TransactionID: r.TransactionID, Problem: r.Problem}
	}
	return out, nil
}

// PurgeFinancialRecords implements Store through 000121's
// financial_records_purge.
//
// cross-org: retention spans every org (finance data).
func (s gormStore) PurgeFinancialRecords(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT financial_records_purge( ?, ?, ?)`, cutoff, max(limit, 1), dryRun).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("ledger: purge financial records: %w", db.Translate(err))
	}
	return int(n), nil
}
