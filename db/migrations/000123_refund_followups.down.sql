-- 000123_refund_followups.down.sql
BEGIN;
SET LOCAL lock_timeout = '5s';
-- Wait for in-flight refund closures before checking their history. Other
-- flows lock transfers -> refunds or collections -> transfers; never wait for
-- those tables while holding refunds. Contention aborts this transaction.
LOCK TABLE refunds IN ACCESS EXCLUSIVE MODE;
LOCK TABLE incoming_bank_transfers, receivable_collections IN ACCESS EXCLUSIVE MODE NOWAIT;
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM receivable_collections) OR EXISTS (SELECT 1 FROM refunds WHERE closed_at IS NOT NULL OR status='cancelled') THEN
  RAISE EXCEPTION 'refund followups contain financial history; migration cannot be rolled back';
 END IF;
END $$;
UPDATE public.retention_policies SET target = 'orders, order_lines, payments, payment_splits, refunds, incoming_bank_transfers, ledger_transactions, ledger_entries, payouts, payout_items, invoices, invoice_lines, legal_parties, legal_party_documents, payees, payee_bank_accounts, files:financial'
 WHERE code = 'financial_records';
CREATE OR REPLACE FUNCTION public.financial_order_ok(p_order uuid, p_cutoff timestamptz) RETURNS boolean
  LANGUAGE sql STABLE
  SET search_path = pg_catalog, public
  AS $$
  WITH txn AS (
    SELECT x.id, x.created_at FROM public.ledger_transactions x
     WHERE (x.ref_type = 'order' AND x.ref_id = p_order)
        OR (x.ref_type = 'payment' AND x.ref_id IN (SELECT p.id FROM public.payments p WHERE p.order_id = p_order))
        OR (x.ref_type = 'refund' AND x.ref_id IN (SELECT r.id FROM public.refunds r WHERE r.order_id = p_order))
        OR EXISTS (SELECT 1 FROM public.ledger_entries e JOIN public.order_lines l ON l.id = e.order_line_id
                    WHERE e.transaction_id = x.id AND l.order_id = p_order)
  )
  SELECT NOT EXISTS (SELECT 1 FROM public.order_lines l WHERE l.order_id = p_order AND greatest(l.created_at, l.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.payments p WHERE p.order_id = p_order AND greatest(p.created_at, p.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.payment_splits s WHERE s.order_id = p_order AND greatest(s.created_at, s.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.refunds r WHERE r.order_id = p_order AND greatest(r.created_at, r.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.incoming_bank_transfers t WHERE t.matched_order_id = p_order
                                                                     AND greatest(t.created_at, t.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.invoices i WHERE i.order_id = p_order AND greatest(i.created_at, i.updated_at) >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM public.invoices i JOIN public.invoices j ON j.refers_to_invoice_id = i.id
                      WHERE i.order_id = p_order AND j.order_id IS DISTINCT FROM p_order)
     AND NOT EXISTS (SELECT 1 FROM txn WHERE txn.created_at >= p_cutoff)
     AND NOT EXISTS (SELECT 1 FROM txn JOIN public.ledger_entries e ON e.transaction_id = txn.id
                                      JOIN public.ledger_accounts a ON a.code = e.account_code
                      WHERE a.per_payee AND e.order_line_id IS NULL)
     AND NOT EXISTS (SELECT 1 FROM public.order_lines l
                      WHERE l.order_id = p_order
                        AND (SELECT coalesce(sum(e.amount), 0) FROM public.ledger_entries e
                               JOIN public.ledger_accounts a ON a.code = e.account_code
                              WHERE e.order_line_id = l.id AND a.per_payee) <> 0)
$$;
REVOKE ALL ON FUNCTION public.financial_order_ok(uuid, timestamptz) FROM PUBLIC;

CREATE OR REPLACE FUNCTION public.financial_records_purge(p_cutoff timestamptz, p_limit integer, p_dry_run boolean) RETURNS bigint
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$
DECLARE
  v_cutoff  timestamptz := public.financial_records_cutoff(p_cutoff, p_limit, p_dry_run);
  v_limit   integer := CASE WHEN p_dry_run THEN NULL ELSE p_limit END;
  v_orders  uuid[];
  v_bad     uuid[];
  v_sel     uuid[];
  v_grow    uuid[];
  v_lines   uuid[];
  v_payouts uuid[];
  v_txns    uuid[];
  v_inv     uuid[];
  v_ids     uuid[];
  v_n       bigint := 0;
  v_k       bigint;
BEGIN
  -- 1. Orders whose own aggregate is closed and old.
  v_orders := ARRAY(SELECT o.id FROM public.orders o
                     WHERE greatest(o.created_at, o.updated_at) < v_cutoff
                       AND public.financial_order_ok(o.id, v_cutoff));
  -- 2. Fixpoint: an order whose line is in a payout that cannot go (young,
  -- or paying a line of an order that cannot go), or whose ledger
  -- transaction or invoice line also touches such an order, cannot go.
  LOOP
    v_bad := ARRAY(
      SELECT DISTINCT l.order_id FROM public.order_lines l
       WHERE l.order_id = ANY (v_orders)
         AND (EXISTS (SELECT 1 FROM public.payout_items i
                       WHERE i.order_line_id = l.id
                         AND (NOT public.financial_payout_ok(i.payout_id, v_cutoff)
                           OR EXISTS (SELECT 1 FROM public.payout_items i2 JOIN public.order_lines l2 ON l2.id = i2.order_line_id
                                       WHERE i2.payout_id = i.payout_id AND l2.order_id <> ALL (v_orders))))
           OR (l.payout_id IS NOT NULL AND NOT public.financial_payout_ok(l.payout_id, v_cutoff))
           OR EXISTS (SELECT 1 FROM public.ledger_entries e
                        JOIN public.ledger_entries e2 ON e2.transaction_id = e.transaction_id
                        JOIN public.order_lines l2 ON l2.id = e2.order_line_id
                       WHERE e.order_line_id = l.id AND l2.order_id <> ALL (v_orders))
           OR EXISTS (SELECT 1 FROM public.invoice_lines il JOIN public.invoices v ON v.id = il.invoice_id
                       WHERE il.order_line_id = l.id
                         AND (v.order_id IS NULL OR v.order_id <> ALL (v_orders))
                         AND (v.payout_id IS NULL
                           OR EXISTS (SELECT 1 FROM public.payout_items i2 JOIN public.order_lines l2 ON l2.id = i2.order_line_id
                                       WHERE i2.payout_id = v.payout_id AND l2.order_id <> ALL (v_orders))))));
    EXIT WHEN cardinality(v_bad) = 0;
    v_orders := ARRAY(SELECT x FROM unnest(v_orders) x WHERE x <> ALL (v_bad));
  END LOOP;

  -- 3. The batch: the oldest p_limit orders, closed over the payouts and
  -- ledger transactions they share (all of them passed the fixpoint).
  IF p_dry_run THEN
    v_sel := v_orders;
  ELSE
    v_sel := ARRAY(SELECT o.id FROM public.orders o WHERE o.id = ANY (v_orders) ORDER BY o.created_at, o.id LIMIT p_limit);
    LOOP
      v_grow := ARRAY(
        SELECT l2.order_id FROM public.order_lines l
          JOIN public.payout_items i ON i.order_line_id = l.id
          JOIN public.payout_items i2 ON i2.payout_id = i.payout_id
          JOIN public.order_lines l2 ON l2.id = i2.order_line_id
         WHERE l.order_id = ANY (v_sel) AND l2.order_id <> ALL (v_sel)
        UNION
        SELECT l2.order_id FROM public.order_lines l
          JOIN public.ledger_entries e ON e.order_line_id = l.id
          JOIN public.ledger_entries e2 ON e2.transaction_id = e.transaction_id
          JOIN public.order_lines l2 ON l2.id = e2.order_line_id
         WHERE l.order_id = ANY (v_sel) AND l2.order_id <> ALL (v_sel)
        UNION
        SELECT l2.order_id FROM public.order_lines l
          JOIN public.invoice_lines il ON il.order_line_id = l.id
          JOIN public.invoice_lines il2 ON il2.invoice_id = il.invoice_id
          JOIN public.order_lines l2 ON l2.id = il2.order_line_id
         WHERE l.order_id = ANY (v_sel) AND l2.order_id <> ALL (v_sel)
        UNION
        SELECT v.order_id FROM public.order_lines l
          JOIN public.invoice_lines il ON il.order_line_id = l.id
          JOIN public.invoices v ON v.id = il.invoice_id
         WHERE l.order_id = ANY (v_sel) AND v.order_id IS NOT NULL AND v.order_id <> ALL (v_sel)
        UNION
        SELECT l2.order_id FROM public.order_lines l
          JOIN public.invoice_lines il ON il.order_line_id = l.id
          JOIN public.invoices v ON v.id = il.invoice_id
          JOIN public.payout_items i2 ON i2.payout_id = v.payout_id
          JOIN public.order_lines l2 ON l2.id = i2.order_line_id
         WHERE l.order_id = ANY (v_sel) AND l2.order_id <> ALL (v_sel));
      EXIT WHEN cardinality(v_grow) = 0;
      v_sel := v_sel || v_grow;
    END LOOP;
  END IF;
  v_lines := ARRAY(SELECT l.id FROM public.order_lines l WHERE l.order_id = ANY (v_sel));
  v_payouts := ARRAY(SELECT i.payout_id FROM public.payout_items i WHERE i.order_line_id = ANY (v_lines)
                     UNION SELECT l.payout_id FROM public.order_lines l WHERE l.id = ANY (v_lines) AND l.payout_id IS NOT NULL);
  v_txns := ARRAY(SELECT x.id FROM public.ledger_transactions x
                   WHERE (x.ref_type = 'order' AND x.ref_id = ANY (v_sel))
                      OR (x.ref_type = 'payment' AND x.ref_id IN (SELECT p.id FROM public.payments p WHERE p.order_id = ANY (v_sel)))
                      OR (x.ref_type = 'refund' AND x.ref_id IN (SELECT r.id FROM public.refunds r WHERE r.order_id = ANY (v_sel)))
                      OR (x.ref_type = 'payout' AND x.ref_id = ANY (v_payouts))
                      OR EXISTS (SELECT 1 FROM public.ledger_entries e WHERE e.transaction_id = x.id AND e.order_line_id = ANY (v_lines)));
  v_inv := ARRAY(WITH RECURSIVE inv (id) AS (
                    SELECT v.id FROM public.invoices v
                     WHERE v.order_id = ANY (v_sel) OR v.payout_id = ANY (v_payouts)
                        OR EXISTS (SELECT 1 FROM public.invoice_lines il WHERE il.invoice_id = v.id AND il.order_line_id = ANY (v_lines))
                    UNION
                    SELECT j.id FROM public.invoices j JOIN inv ON j.refers_to_invoice_id = inv.id)
                  SELECT inv.id FROM inv);

  IF p_dry_run THEN
    v_n := cardinality(v_sel) + cardinality(v_lines) + cardinality(v_payouts) + cardinality(v_txns) + cardinality(v_inv)
         + (SELECT count(*) FROM public.payments p WHERE p.order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.payment_splits s WHERE s.order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.refunds r WHERE r.order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.incoming_bank_transfers t WHERE t.matched_order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.invoice_lines il WHERE il.invoice_id = ANY (v_inv))
         + (SELECT count(*) FROM public.ledger_entries e WHERE e.transaction_id = ANY (v_txns))
         + (SELECT count(*) FROM public.payout_items i WHERE i.payout_id = ANY (v_payouts));
  ELSIF cardinality(v_sel) > 0 THEN
    -- 4. Leaves first; the payout cycle (order_lines ↔ payout_items ↔
    -- payouts) goes in one statement, its foreign keys are checked at the
    -- statement's end.
    DELETE FROM public.invoice_lines WHERE invoice_id = ANY (v_inv);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.invoices WHERE id = ANY (v_inv);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    PERFORM set_config('koro.retention_purge', 'public.ledger_entries', true);
    DELETE FROM public.ledger_entries WHERE transaction_id = ANY (v_txns);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    PERFORM set_config('koro.retention_purge', 'public.ledger_transactions', true);
    DELETE FROM public.ledger_transactions WHERE id = ANY (v_txns);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    PERFORM set_config('koro.retention_purge', '', true);
    DELETE FROM public.payment_splits WHERE order_id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.refunds WHERE order_id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    PERFORM set_config('koro.retention_purge', 'public.payout_items', true);
    WITH di AS (DELETE FROM public.payout_items WHERE payout_id = ANY (v_payouts) RETURNING 1),
         dl AS (DELETE FROM public.order_lines WHERE id = ANY (v_lines) RETURNING 1),
         dp AS (DELETE FROM public.payouts WHERE id = ANY (v_payouts) RETURNING 1)
    SELECT (SELECT count(*) FROM di) + (SELECT count(*) FROM dl) + (SELECT count(*) FROM dp) INTO v_k;
    v_n := v_n + v_k;
    PERFORM set_config('koro.retention_purge', '', true);
    DELETE FROM public.payments WHERE order_id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.incoming_bank_transfers WHERE matched_order_id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.orders WHERE id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
  END IF;

  -- 5. Loose and master rows nothing refers to any more.
  v_ids := ARRAY(SELECT p.id FROM public.payouts p
                  WHERE p.status IN ('failed','cancelled') AND greatest(p.created_at, p.updated_at) < v_cutoff
                    AND NOT EXISTS (SELECT 1 FROM public.payout_items i WHERE i.payout_id = p.id)
                    AND NOT EXISTS (SELECT 1 FROM public.order_lines l WHERE l.payout_id = p.id)
                    AND NOT EXISTS (SELECT 1 FROM public.invoices v WHERE v.payout_id = p.id)
                    AND NOT EXISTS (SELECT 1 FROM public.ledger_transactions x WHERE x.ref_type = 'payout' AND x.ref_id = p.id)
                  ORDER BY p.created_at, p.id LIMIT v_limit);
  v_n := v_n + cardinality(v_ids);
  IF NOT p_dry_run THEN
    DELETE FROM public.payouts WHERE id = ANY (v_ids);
  END IF;

  v_ids := ARRAY(SELECT t.id FROM public.incoming_bank_transfers t
                  WHERE t.matched_order_id IS NULL AND greatest(t.created_at, t.updated_at) < v_cutoff
                  ORDER BY t.created_at, t.id LIMIT v_limit);
  v_n := v_n + cardinality(v_ids);
  IF NOT p_dry_run THEN
    DELETE FROM public.incoming_bank_transfers WHERE id = ANY (v_ids);
  END IF;

  v_ids := ARRAY(SELECT v.id FROM public.invoices v
                  WHERE v.order_id IS NULL AND v.payout_id IS NULL AND greatest(v.created_at, v.updated_at) < v_cutoff
                    AND NOT EXISTS (SELECT 1 FROM public.invoices j WHERE j.refers_to_invoice_id = v.id)
                    AND NOT EXISTS (SELECT 1 FROM public.invoice_lines il WHERE il.invoice_id = v.id AND il.order_line_id IS NOT NULL)
                  ORDER BY v.created_at, v.id LIMIT v_limit);
  IF p_dry_run THEN
    v_n := v_n + cardinality(v_ids) + (SELECT count(*) FROM public.invoice_lines il WHERE il.invoice_id = ANY (v_ids));
  ELSE
    DELETE FROM public.invoice_lines WHERE invoice_id = ANY (v_ids);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.invoices WHERE id = ANY (v_ids);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
  END IF;

  v_ids := ARRAY(SELECT b.id FROM public.payee_bank_accounts b
                  WHERE b.status IN ('superseded','rejected','revoked') AND greatest(b.created_at, b.updated_at) < v_cutoff
                    AND NOT EXISTS (SELECT 1 FROM public.payouts p WHERE p.bank_account_id = b.id)
                    AND NOT EXISTS (SELECT 1 FROM public.payee_bank_accounts n WHERE n.superseded_by_id = b.id)
                  ORDER BY b.created_at, b.id LIMIT v_limit);
  v_n := v_n + cardinality(v_ids);
  IF NOT p_dry_run THEN
    DELETE FROM public.payee_bank_accounts WHERE id = ANY (v_ids);
  END IF;

  v_ids := ARRAY(SELECT y.id FROM public.payees y
                  WHERE y.status IN ('draft','closed') AND greatest(y.created_at, y.updated_at) < v_cutoff
                    AND NOT EXISTS (SELECT 1 FROM public.payee_bank_accounts b WHERE b.payee_id = y.id)
                    AND NOT EXISTS (SELECT 1 FROM public.offerings f WHERE f.default_payee_id = y.id)
                    AND NOT EXISTS (SELECT 1 FROM public.order_lines l WHERE l.payee_id = y.id)
                    AND NOT EXISTS (SELECT 1 FROM public.ledger_entries e WHERE e.payee_id = y.id)
                    AND NOT EXISTS (SELECT 1 FROM public.payouts p WHERE p.payee_id = y.id)
                    AND NOT EXISTS (SELECT 1 FROM public.payout_items i WHERE i.payee_id = y.id)
                  ORDER BY y.created_at, y.id LIMIT v_limit);
  v_n := v_n + cardinality(v_ids);
  IF NOT p_dry_run THEN
    DELETE FROM public.payees WHERE id = ANY (v_ids);
  END IF;

  v_ids := ARRAY(SELECT lp.id FROM public.legal_parties lp
                  WHERE lp.kind <> 'platform' AND greatest(lp.created_at, lp.updated_at) < v_cutoff
                    AND NOT EXISTS (SELECT 1 FROM public.legal_party_documents d
                                     WHERE d.legal_party_id = lp.id AND greatest(d.created_at, d.verified_at) >= v_cutoff)
                    AND NOT EXISTS (SELECT 1 FROM public.payees y WHERE y.legal_party_id = lp.id)
                    AND NOT EXISTS (SELECT 1 FROM public.orders o WHERE o.buyer_party_id = lp.id)
                    AND NOT EXISTS (SELECT 1 FROM public.invoices v WHERE v.issuer_party_id = lp.id OR v.recipient_party_id = lp.id)
                    AND NOT EXISTS (SELECT 1 FROM public.collection_accounts c WHERE c.legal_party_id = lp.id)
                    AND NOT EXISTS (SELECT 1 FROM public.provider_contact_map m WHERE m.legal_party_id = lp.id)
                  ORDER BY lp.created_at, lp.id LIMIT v_limit);
  IF p_dry_run THEN
    v_n := v_n + cardinality(v_ids) + (SELECT count(*) FROM public.legal_party_documents d WHERE d.legal_party_id = ANY (v_ids));
  ELSE
    DELETE FROM public.legal_party_documents WHERE legal_party_id = ANY (v_ids);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.legal_parties WHERE id = ANY (v_ids);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
  END IF;
  RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION public.financial_records_purge(timestamptz, integer, boolean) FROM PUBLIC;
DROP TRIGGER incoming_bank_transfers_collection_consistent ON incoming_bank_transfers;
DROP FUNCTION public.incoming_transfer_check_collection();
DROP TABLE receivable_collections;
DROP FUNCTION public.receivable_collections_check();
ALTER TABLE incoming_bank_transfers DROP CONSTRAINT incoming_bank_transfers_collection_source_uq,
 DROP CONSTRAINT incoming_bank_transfers_status_chk,
 ADD CONSTRAINT incoming_bank_transfers_status_chk CHECK (status IN ('unmatched','matched','ignored','return_requested','return_approved','returned'));
DROP TRIGGER refunds_closure_guard ON refunds;
DROP TRIGGER refunds_closure_frozen ON refunds;
DROP FUNCTION public.refunds_guard_closure();
ALTER TABLE refunds DROP CONSTRAINT refunds_closure_four_eyes_chk,DROP CONSTRAINT refunds_closure_chk,
 DROP CONSTRAINT refunds_status_chk,DROP CONSTRAINT refunds_approved_chk,
 DROP COLUMN closed_by,DROP COLUMN closed_at,DROP COLUMN cancel_reason,
 ADD CONSTRAINT refunds_status_chk CHECK (status IN ('requested','approved','processing','completed','failed','rejected')),
 ADD CONSTRAINT refunds_approved_chk CHECK ((status IN ('approved','processing','completed','failed')) = (approved_by IS NOT NULL)
 AND (status IN ('approved','processing','completed','failed')) = (approved_at IS NOT NULL));
COMMIT;
