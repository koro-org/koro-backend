//go:build integration

package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

type collectionWorld struct{ order, line, source, collection, requester uuid.UUID }

func (w *p1aWorld) receivableCollection(collected bool) collectionWorld {
	w.t.Helper()
	s := w.shop()
	order, line, _ := w.settledOrder(s)
	debt := uuid.NewV7()
	w.mustCommit(st(`INSERT INTO ledger_transactions(id,kind,ref_type,ref_id,occurred_at) VALUES($1,'adjustment','manual',$1,now())`, debt),
		st(`INSERT INTO ledger_entries(transaction_id,account_code,order_line_id,amount) VALUES($1,'payee_receivable',$2,1000),($1,'koro_bank',$2,-1000)`, debt, line))
	receipt := w.file("transfer_receipt")
	source := w.transfer(w.collectionAccount(), map[string]string{"amount": "1000", "receipt_file_id": "'" + receipt.String() + "'"})
	c, requester := uuid.NewV7(), w.user()
	w.mustCommit(st(`INSERT INTO receivable_collections(id,incoming_transfer_id,order_line_id,order_id,payee_id,org_id,collection_account_id,amount,bank_reference,receipt_file_id,requested_by)
 SELECT $1,t.id,$3,$4,$5,$6,t.collection_account_id,t.amount,t.bank_reference,t.receipt_file_id,$7 FROM incoming_bank_transfers t WHERE t.id=$2`, c, source, line, order, s.seller.payee, s.seller.org, requester),
		st(`UPDATE incoming_bank_transfers SET status='collection_requested' WHERE id=$1`, source))
	if collected {
		txn := uuid.NewV7()
		w.mustCommit(st(`INSERT INTO ledger_transactions(id,kind,ref_type,ref_id,occurred_at) VALUES($1,'adjustment','manual',$2,now())`, txn, c),
			st(`INSERT INTO ledger_entries(transaction_id,account_code,order_line_id,amount) VALUES($1,'koro_bank',$2,1000),($1,'payee_receivable',$2,-1000)`, txn, line),
			st(`UPDATE receivable_collections SET status='collected',approved_by=$2,approved_at=now(),ledger_transaction_id=$3 WHERE id=$1`, c, w.user(), txn),
			st(`UPDATE incoming_bank_transfers SET status='collected' WHERE id=$1`, source))
	}
	return collectionWorld{order, line, source, c, requester}
}

func TestSchema_ReceivableOwnershipChecksAndFourEyes(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	c := w.receivableCollection(false)
	expectCommit(t, w, "same transfer twice", db.CodeUniqueViolation, "",
		st(`INSERT INTO receivable_collections(id,incoming_transfer_id,order_line_id,order_id,payee_id,org_id,collection_account_id,amount,bank_reference,receipt_file_id,requested_by)
 SELECT $2,incoming_transfer_id,order_line_id,order_id,payee_id,org_id,collection_account_id,amount,bank_reference,receipt_file_id,requested_by FROM receivable_collections WHERE id=$1`, c.collection, uuid.NewV7()))
	wrong := uuid.NewV7()
	expectCommit(t, w, "cash belongs to this collection", db.CodeCheckViolation, "receivable_collections_ledger_chk",
		st(`INSERT INTO ledger_transactions(id,kind,ref_type,ref_id,occurred_at)VALUES($1,'adjustment','manual',$1,now())`, wrong),
		st(`INSERT INTO ledger_entries(transaction_id,account_code,order_line_id,amount)VALUES($1,'koro_bank',$2,1000),($1,'payee_receivable',$2,-1000)`, wrong, c.line),
		st(`UPDATE receivable_collections SET status='collected',approved_by=$2,approved_at=now(),ledger_transaction_id=$3 WHERE id=$1`, c.collection, w.user(), wrong),
		st(`UPDATE incoming_bank_transfers SET status='collected' WHERE id=$1`, c.source))
	expectCommit(t, w, "source cannot escape", db.CodeCheckViolation, "receivable_collections_source_state_chk",
		st(`UPDATE incoming_bank_transfers SET status='unmatched' WHERE id=$1`, c.source))
	expectFail(t, w.d.Pool, []p1aCase{
		{"requester approves", `UPDATE receivable_collections SET status='collected',approved_by=$2,approved_at=now(),ledger_transaction_id=$3 WHERE id=$1`, []any{c.collection, c.requester, uuid.NewV7()}, db.CodeCheckViolation, "receivable_collections_four_eyes_chk"},
		{"amount frozen", `UPDATE receivable_collections SET amount=1001 WHERE id=$1`, []any{c.collection}, db.CodeRestrictViolation, ""},
		{"app deletes evidence", `DELETE FROM receivable_collections WHERE id=$1`, []any{c.collection}, db.CodeInsufficientPriv, ""},
	})
	// Rejection releases this same source without erasing the original trail.
	w.mustCommit(st(`UPDATE receivable_collections SET status='rejected',rejected_by=$2,rejected_at=now(),rejection_reason='Synthetic correction' WHERE id=$1`, c.collection, w.user()),
		st(`UPDATE incoming_bank_transfers SET status='unmatched' WHERE id=$1`, c.source))
	next := uuid.NewV7()
	w.mustCommit(st(`INSERT INTO receivable_collections(id,incoming_transfer_id,order_line_id,order_id,payee_id,org_id,collection_account_id,amount,bank_reference,receipt_file_id,requested_by)
 SELECT $2,incoming_transfer_id,order_line_id,order_id,payee_id,org_id,collection_account_id,amount,bank_reference,receipt_file_id,requested_by FROM receivable_collections WHERE id=$1`, c.collection, next),
		st(`UPDATE incoming_bank_transfers SET status='collection_requested' WHERE id=$1`, c.source))
}

func TestSchema_ReceivableRetentionWholeAggregate(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)
	pending := w.receivableCollection(false)
	young := w.receivableCollection(true)
	old := w.receivableCollection(true)
	// An unpaid debt with no requested collection must also retain its order.
	debt := w.receivableCollection(false)
	w.mustCommit(st(`UPDATE receivable_collections SET status='rejected',rejected_by=$2,rejected_at=now(),rejection_reason='Synthetic correction' WHERE id=$1`, debt.collection, w.user()),
		st(`UPDATE incoming_bank_transfers SET status='unmatched' WHERE id=$1`, debt.source))
	ageAll(t, owner, "12 years")
	w.exec(`UPDATE receivable_collections SET updated_at=now() WHERE id=$1`, young.collection)
	w.approve("financial_records")
	dry, state := w.purgeAt("financial_records_purge", 100, true)
	if state != "" || dry == 0 {
		t.Fatalf("dry=%d %s", dry, state)
	}
	for range 3 {
		_, state = w.purgeAt("financial_records_purge", 1, false)
		if state != "" {
			t.Fatalf("purge=%s", state)
		}
	}
	if w.count(`SELECT count(*) FROM orders WHERE id=$1`, old.order) != 0 || w.count(`SELECT count(*) FROM incoming_bank_transfers WHERE id=$1`, old.source) != 0 || w.count(`SELECT count(*) FROM receivable_collections WHERE id=$1`, old.collection) != 0 {
		t.Fatal("old collected aggregate survived")
	}
	for _, c := range []collectionWorld{pending, young, debt} {
		if w.count(`SELECT count(*) FROM orders WHERE id=$1`, c.order) != 1 || w.count(`SELECT count(*) FROM incoming_bank_transfers WHERE id=$1`, c.source) != 1 || w.count(`SELECT count(*) FROM receivable_collections WHERE id=$1`, c.collection) != 1 {
			t.Fatal("open debt or young evidence lost aggregate")
		}
	}
}

// One rejected source may later belong to a different collection order or a
// matched payment. A young member protects both orders; an old connected
// component is purged whole even when p_limit is one.
func TestSchema_ReceivableRetentionSharedSourceGraph(t *testing.T) {
	t.Parallel()
	for _, flow := range []string{"collection", "matched"} {
		t.Run(flow, func(t *testing.T) {
			t.Parallel()
			w := newP1aWorld(t)
			owner := testutil.OwnerConn(t, w.d)
			first := w.receivableCollection(false)
			w.mustCommit(st(`UPDATE receivable_collections SET status='rejected',rejected_by=$2,rejected_at=now(),rejection_reason='Wrong source line' WHERE id=$1`, first.collection, w.user()),
				st(`UPDATE incoming_bank_transfers SET status='unmatched' WHERE id=$1`, first.source))
			// First line's debt is settled independently, leaving its rejected receipt
			// as the only cross-order edge.
			x := uuid.NewV7()
			w.mustCommit(st(`INSERT INTO ledger_transactions(id,kind,ref_type,ref_id,occurred_at)VALUES($1,'adjustment','manual',$1,now())`, x),
				st(`INSERT INTO ledger_entries(transaction_id,account_code,order_line_id,amount)VALUES($1,'payee_receivable',$2,-1000),($1,'koro_bank',$2,1000)`, x, first.line))
			s := w.shop()
			second, line, _ := w.settledOrder(s)
			if flow == "collection" {
				x = uuid.NewV7()
				w.mustCommit(st(`INSERT INTO ledger_transactions(id,kind,ref_type,ref_id,occurred_at)VALUES($1,'adjustment','manual',$1,now())`, x),
					st(`INSERT INTO ledger_entries(transaction_id,account_code,order_line_id,amount)VALUES($1,'payee_receivable',$2,1000),($1,'koro_bank',$2,-1000)`, x, line))
				younger := uuid.NewV7()
				w.mustCommit(st(`INSERT INTO receivable_collections(id,incoming_transfer_id,order_line_id,order_id,payee_id,org_id,collection_account_id,amount,bank_reference,receipt_file_id,requested_by)
 SELECT $1,t.id,$3,$4,$5,$6,t.collection_account_id,t.amount,t.bank_reference,t.receipt_file_id,$7 FROM incoming_bank_transfers t WHERE t.id=$2`, younger, first.source, line, second, s.seller.payee, s.seller.org, w.user()),
					st(`UPDATE incoming_bank_transfers SET status='collection_requested' WHERE id=$1`, first.source))
				x = uuid.NewV7()
				w.mustCommit(st(`INSERT INTO ledger_transactions(id,kind,ref_type,ref_id,occurred_at)VALUES($1,'adjustment','manual',$2,now())`, x, younger),
					st(`INSERT INTO ledger_entries(transaction_id,account_code,order_line_id,amount)VALUES($1,'payee_receivable',$2,-1000),($1,'koro_bank',$2,1000)`, x, line),
					st(`UPDATE receivable_collections SET status='collected',approved_by=$2,approved_at=now(),ledger_transaction_id=$3 WHERE id=$1`, younger, w.user(), x),
					st(`UPDATE incoming_bank_transfers SET status='collected' WHERE id=$1`, first.source))
			} else {
				w.exec(`UPDATE incoming_bank_transfers SET status='matched',matched_order_id=$2,matched_by=$3,matched_at=now() WHERE id=$1`, first.source, second, w.user())
				w.exec(`INSERT INTO payments(id,order_id,org_id,provider,incoming_transfer_id,amount,status,captured_at,captured_by)VALUES($1,$2,$3,'bank_transfer',$4,1000,'captured',now(),$5)`, uuid.NewV7(), second, s.buyer.org, first.source, w.user())
			}
			ageAll(t, owner, "12 years")
			// Updating the second order is sufficient for either kind of graph edge.
			w.exec(`UPDATE orders SET updated_at=now() WHERE id=$1`, second)
			w.approve("financial_records")
			if _, state := w.purgeAt("financial_records_purge", 1, false); state != "" {
				t.Fatalf("young purge=%s", state)
			}
			if w.count(`SELECT count(*) FROM orders WHERE id IN ($1,$2)`, first.order, second) != 2 || w.count(`SELECT count(*) FROM incoming_bank_transfers WHERE id=$1`, first.source) != 1 {
				t.Fatal("shared young graph partly purged")
			}
			ageAll(t, owner, "12 years")
			if _, state := w.purgeAt("financial_records_purge", 1, false); state != "" {
				t.Fatalf("old purge=%s", state)
			}
			if w.count(`SELECT count(*) FROM orders WHERE id IN ($1,$2)`, first.order, second) != 0 || w.count(`SELECT count(*) FROM incoming_bank_transfers WHERE id=$1`, first.source) != 0 {
				t.Fatal("shared old graph not closed over limit=1")
			}
		})
	}
}

func TestSchema_RefundFollowupsPreservesLegacyAndRefusesLossyDown(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)
	if w.count(`SELECT count(*) FROM retention_policies WHERE code='financial_records' AND target LIKE '%receivable_collections%'`) != 1 {
		t.Fatal("collection missing from financial retention policy")
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000123_refund_followups.down.sql")); err != nil {
		t.Fatal(err)
	}
	if w.count(`SELECT count(*) FROM retention_policies WHERE code='financial_records' AND target LIKE '%receivable_collections%'`) != 0 {
		t.Fatal("down retained the collection policy target")
	}
	s := w.shop()
	order, line := w.payableLine(s)
	payment := w.payment(order, s.buyer.org)
	a, b := w.user(), w.user()
	legacy := uuid.NewV7()
	w.exec(`INSERT INTO refunds(id,payment_id,order_id,order_line_id,amount,reason,requested_by,method,target_iban)VALUES($1,$2,$3,$4,1000,'not_fulfilled',$5,'bank_eft',$6)`, legacy, payment, order, line, a, w.iban())
	w.exec(`UPDATE refunds SET status='approved',approved_by=$2,approved_at=now() WHERE id=$1`, legacy, b)
	w.exec(`UPDATE refunds SET status='failed',failure_reason='Legacy bank failure',bank_reference='SYNTHETIC-LEGACY' WHERE id=$1`, legacy)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000123_refund_followups.up.sql")); err != nil {
		t.Fatal(err)
	}
	if w.count(`SELECT count(*) FROM refunds WHERE id=$1 AND status='failed' AND approved_by=$2 AND failure_reason='Legacy bank failure' AND bank_reference='SYNTHETIC-LEGACY' AND closed_at IS NULL`, legacy, b) != 1 {
		t.Fatal("legacy refund was rewritten")
	}
	c := w.receivableCollection(false)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000123_refund_followups.down.sql")); err == nil {
		t.Fatal("down discarded collection financial history")
	}
	if _, err := owner.Exec(t.Context(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if w.count(`SELECT count(*) FROM receivable_collections WHERE id=$1`, c.collection) != 1 {
		t.Fatal("failed rollback lost collection")
	}
}

// A closure uncommitted when down starts must become visible to its history
// guard after the writer commits. Checking before the DDL lock loses evidence.
func TestSchema_RefundFollowupsDownWaitsForConcurrentClosure(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)
	s := w.shop()
	order, line := w.payableLine(s)
	payment := w.payment(order, s.buyer.org)
	requester, closer := w.user(), w.user()
	refund := uuid.NewV7()
	w.exec(`INSERT INTO refunds(id,payment_id,order_id,order_line_id,amount,reason,requested_by,method,target_iban)VALUES($1,$2,$3,$4,1000,'not_fulfilled',$5,'bank_eft',$6)`, refund, payment, order, line, requester, w.iban())
	w.exec(`UPDATE refunds SET status='approved',approved_by=$2,approved_at=now() WHERE id=$1`, refund, closer)
	w.exec(`UPDATE retention_policies SET retention=interval '13 years',enabled=false,legal_approved=true,approved_ref='SYNTHETIC-RETENTION-REVIEW',updated_by=$1 WHERE code='financial_records'`, closer)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	writer, err := w.d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err = writer.Exec(ctx, `UPDATE refunds SET status='failed',failure_reason='Synthetic EFT stopped',closed_by=$2,closed_at=now() WHERE id=$1`, refund, closer); err != nil {
		t.Fatal(err)
	}
	var writerPID, downPID int
	if err = writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	if err = owner.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&downPID); err != nil {
		t.Fatal(err)
	}
	downSQL := mustRead(t, "000123_refund_followups.down.sql")
	done := make(chan error, 1)
	go func() { _, downErr := owner.Exec(ctx, downSQL); done <- downErr }()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		err = w.d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation='refunds'::regclass AND mode='AccessExclusiveLock' AND NOT granted) AND $2=ANY(pg_blocking_pids($1))`, downPID, writerPID).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err = <-done:
			t.Fatalf("down did not wait for the closure: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	if err = writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err == nil || !strings.Contains(err.Error(), "financial history") {
			t.Fatalf("down discarded concurrent closure or failed before its history check: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err = owner.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if w.count(`SELECT count(*) FROM refunds WHERE id=$1 AND status='failed' AND closed_by=$2 AND closed_at IS NOT NULL AND approved_by=$2`, refund, closer) != 1 {
		t.Fatal("closure evidence lost during concurrent down")
	}
	if w.count(`SELECT count(*) FROM retention_policies WHERE code='financial_records' AND target LIKE '%receivable_collections%' AND retention=interval '13 years' AND NOT enabled AND legal_approved AND approved_ref='SYNTHETIC-RETENTION-REVIEW' AND updated_by=$1`, closer) != 1 {
		t.Fatal("down changed the policy target or approval metadata")
	}
}

// Match holds the source before inserting an automatic refund; a collection
// decision holds its request before updating the source. Down must give up its
// locks on contention so either writer can finish in its normal lock order.
func TestSchema_RefundFollowupsDownRejectsOppositeLockOrders(t *testing.T) {
	t.Parallel()
	for _, flow := range []string{"match", "collection"} {
		t.Run(flow, func(t *testing.T) {
			t.Parallel()
			w := newP1aWorld(t)
			owner := testutil.OwnerConn(t, w.d)
			actor := w.user()
			var source, collection, payment, order uuid.UUID
			if flow == "match" {
				s := w.shop()
				order, _ = w.payableLine(s)
				payment = w.payment(order, s.buyer.org)
				source = w.transfer(w.collectionAccount(), nil)
			} else {
				c := w.receivableCollection(false)
				source, collection = c.source, c.collection
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			writer, err := w.d.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Rollback(ctx) }()
			lockSQL, lockID := `SELECT id FROM incoming_bank_transfers WHERE id=$1 FOR UPDATE`, source
			if flow == "collection" {
				lockSQL, lockID = `SELECT id FROM receivable_collections WHERE id=$1 FOR UPDATE`, collection
			}
			if _, err = writer.Exec(ctx, lockSQL, lockID); err != nil {
				t.Fatal(err)
			}
			_, err = owner.Exec(ctx, mustRead(t, "000123_refund_followups.down.sql"))
			if db.SQLState(err) != "55P03" || !strings.Contains(err.Error(), "could not obtain lock") {
				t.Fatalf("down must reject table contention without waiting: %v", err)
			}
			if _, err = owner.Exec(ctx, `ROLLBACK`); err != nil {
				t.Fatal(err)
			}
			if flow == "match" {
				// This is the second lock in MatchTransfer's overpayment path.
				_, err = writer.Exec(ctx, `INSERT INTO refunds(id,payment_id,order_id,amount,reason,requested_by,method,target_iban)VALUES($1,$2,$3,1000,'overpayment',$4,'bank_eft',$5)`, uuid.NewV7(), payment, order, actor, w.iban())
			} else {
				_, err = writer.Exec(ctx, `UPDATE receivable_collections SET status='rejected',rejected_by=$2,rejected_at=now(),rejection_reason='Synthetic correction' WHERE id=$1`, collection, actor)
				if err == nil {
					_, err = writer.Exec(ctx, `UPDATE incoming_bank_transfers SET status='unmatched' WHERE id=$1`, source)
				}
			}
			if err != nil {
				t.Fatalf("writer after down rollback: %v", err)
			}
			if err = writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if flow == "match" && w.count(`SELECT count(*) FROM refunds WHERE payment_id=$1 AND reason='overpayment'`, payment) != 1 {
				t.Fatal("match refund did not commit")
			}
			if flow == "collection" && w.count(`SELECT count(*) FROM receivable_collections c JOIN incoming_bank_transfers t ON t.id=c.incoming_transfer_id WHERE c.id=$1 AND c.status='rejected' AND t.status='unmatched'`, collection) != 1 {
				t.Fatal("collection decision did not commit")
			}
			if w.count(`SELECT count(*) FROM retention_policies WHERE code='financial_records' AND target LIKE '%receivable_collections%'`) != 1 {
				t.Fatal("refused down changed the policy target")
			}
		})
	}
}
