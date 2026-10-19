-- 000123_refund_followups.up.sql
-- BE-068: manual clawback collection claims a recorded bank transfer;
-- approved refunds may close before completion without changing money.
BEGIN;
SET LOCAL lock_timeout = '5s';

ALTER TABLE refunds
  ADD COLUMN closed_by uuid CONSTRAINT refunds_closed_by_fkey REFERENCES users(id),
  ADD COLUMN closed_at timestamptz,
  ADD COLUMN cancel_reason text,
  DROP CONSTRAINT refunds_status_chk,
  DROP CONSTRAINT refunds_approved_chk,
  ADD CONSTRAINT refunds_status_chk CHECK (status IN ('requested','approved','processing','completed','failed','rejected','cancelled')),
  ADD CONSTRAINT refunds_approved_chk CHECK (
    (status IN ('approved','processing','completed','failed','cancelled')) = (approved_by IS NOT NULL)
    AND (status IN ('approved','processing','completed','failed','cancelled')) = (approved_at IS NOT NULL)),
  ADD CONSTRAINT refunds_closure_four_eyes_chk CHECK (closed_by IS NULL OR closed_by <> requested_by),
  ADD CONSTRAINT refunds_closure_chk CHECK (
    (closed_by IS NULL) = (closed_at IS NULL)
    AND (closed_at IS NULL OR status IN ('cancelled','failed'))
    AND (status <> 'cancelled' OR closed_at IS NOT NULL)
    AND (status = 'cancelled') = (cancel_reason IS NOT NULL)
    AND (cancel_reason IS NULL OR char_length(btrim(cancel_reason)) BETWEEN 1 AND 500));
CREATE FUNCTION public.refunds_guard_closure() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
  IF OLD.status IN ('failed','cancelled') AND NEW.status IS DISTINCT FROM OLD.status THEN
    RAISE EXCEPTION 'a closed refund is final' USING ERRCODE = 'check_violation', CONSTRAINT = 'refunds_closure_chk';
  END IF;
  IF NEW.status IN ('failed','cancelled') AND NEW.status IS DISTINCT FROM OLD.status THEN
    IF OLD.status <> 'approved' OR NEW.closed_by IS NULL OR NEW.closed_at IS NULL THEN
      RAISE EXCEPTION 'only an approved refund may close with a recorded actor' USING ERRCODE = 'check_violation', CONSTRAINT = 'refunds_closure_chk';
    END IF;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER refunds_closure_guard BEFORE UPDATE ON refunds FOR EACH ROW EXECUTE FUNCTION public.refunds_guard_closure();
CREATE TRIGGER refunds_closure_frozen BEFORE UPDATE ON refunds FOR EACH ROW
 EXECUTE FUNCTION public.koro_freeze_columns('closed_at','status','closed_at','closed_by','cancel_reason','failure_reason');

ALTER TABLE incoming_bank_transfers
 DROP CONSTRAINT incoming_bank_transfers_status_chk,
 ADD CONSTRAINT incoming_bank_transfers_status_chk CHECK (status IN ('unmatched','matched','ignored','return_requested','return_approved','returned','collection_requested','collected')),
 ADD CONSTRAINT incoming_bank_transfers_collection_source_uq UNIQUE (id,collection_account_id,amount,bank_reference,receipt_file_id);

CREATE TABLE receivable_collections (
 id uuid PRIMARY KEY DEFAULT uuidv7(),
 incoming_transfer_id uuid NOT NULL,
 order_line_id uuid NOT NULL,
 order_id uuid NOT NULL,
 payee_id uuid NOT NULL,
 org_id uuid NOT NULL,
 collection_account_id uuid NOT NULL,
 amount bigint NOT NULL CONSTRAINT receivable_collections_amount_chk CHECK (amount > 0),
 bank_reference text NOT NULL CONSTRAINT receivable_collections_reference_chk CHECK (char_length(btrim(bank_reference)) BETWEEN 1 AND 100),
 receipt_file_id uuid NOT NULL CONSTRAINT receivable_collections_receipt_file_id_fkey REFERENCES files(id),
 status text NOT NULL DEFAULT 'requested' CONSTRAINT receivable_collections_status_chk CHECK (status IN ('requested','collected','rejected')),
 requested_by uuid NOT NULL CONSTRAINT receivable_collections_requested_by_fkey REFERENCES users(id),
 approved_by uuid CONSTRAINT receivable_collections_approved_by_fkey REFERENCES users(id),
 approved_at timestamptz,
 rejected_by uuid CONSTRAINT receivable_collections_rejected_by_fkey REFERENCES users(id),
 rejected_at timestamptz,
 rejection_reason text,
 ledger_transaction_id uuid CONSTRAINT receivable_collections_ledger_transaction_id_fkey REFERENCES ledger_transactions(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT receivable_collections_source_fkey FOREIGN KEY (incoming_transfer_id,collection_account_id,amount,bank_reference,receipt_file_id)
   REFERENCES incoming_bank_transfers(id,collection_account_id,amount,bank_reference,receipt_file_id),
 CONSTRAINT receivable_collections_line_order_fkey FOREIGN KEY (order_line_id,order_id) REFERENCES order_lines(id,order_id),
 CONSTRAINT receivable_collections_line_payee_fkey FOREIGN KEY (order_line_id,payee_id) REFERENCES order_lines(id,payee_id),
 CONSTRAINT receivable_collections_payee_org_fkey FOREIGN KEY (payee_id,org_id) REFERENCES payees(id,org_id),
 CONSTRAINT receivable_collections_four_eyes_chk CHECK (approved_by IS NULL OR approved_by <> requested_by),
 CONSTRAINT receivable_collections_approved_chk CHECK (
   (status='collected') = (approved_by IS NOT NULL) AND (status='collected') = (approved_at IS NOT NULL)
   AND (status='collected') = (ledger_transaction_id IS NOT NULL)),
 CONSTRAINT receivable_collections_rejected_chk CHECK (
   (status='rejected') = (rejected_by IS NOT NULL) AND (status='rejected') = (rejected_at IS NOT NULL)
   AND (status='rejected') = (rejection_reason IS NOT NULL)
   AND (rejection_reason IS NULL OR char_length(btrim(rejection_reason)) BETWEEN 1 AND 500))
);
CREATE UNIQUE INDEX receivable_collections_transfer_uq ON receivable_collections(incoming_transfer_id) WHERE status IN ('requested','collected');
CREATE UNIQUE INDEX receivable_collections_receipt_uq ON receivable_collections(receipt_file_id) WHERE status IN ('requested','collected');
CREATE INDEX receivable_collections_line_idx ON receivable_collections(order_line_id);
CREATE INDEX receivable_collections_order_idx ON receivable_collections(order_id);
CREATE INDEX receivable_collections_payee_idx ON receivable_collections(payee_id,created_at DESC,id DESC);
CREATE INDEX receivable_collections_pending_idx ON receivable_collections(created_at,id) WHERE status='requested';
CREATE TRIGGER receivable_collections_touch_updated_at BEFORE UPDATE ON receivable_collections FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER receivable_collections_initial_status BEFORE INSERT ON receivable_collections FOR EACH ROW EXECUTE FUNCTION public.koro_initial_status('requested');
CREATE TRIGGER receivable_collections_frozen BEFORE UPDATE ON receivable_collections FOR EACH ROW
 EXECUTE FUNCTION public.koro_freeze_columns('id','incoming_transfer_id','order_line_id','order_id','payee_id','org_id','collection_account_id','amount','bank_reference','receipt_file_id','requested_by','created_at');
CREATE TRIGGER receivable_collections_approved_frozen BEFORE UPDATE ON receivable_collections FOR EACH ROW
 EXECUTE FUNCTION public.koro_freeze_columns('approved_at','status','approved_by','approved_at','ledger_transaction_id');
CREATE TRIGGER receivable_collections_rejected_frozen BEFORE UPDATE ON receivable_collections FOR EACH ROW
 EXECUTE FUNCTION public.koro_freeze_columns('rejected_at','status','rejected_by','rejected_at','rejection_reason');

-- Deferred ownership checks allow the transfer and collection to change in
-- either statement order while requiring one coherent result at commit.
CREATE FUNCTION public.receivable_collections_check() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE c record; t record; debt numeric; reserved numeric;
BEGIN
  SELECT * INTO c FROM public.receivable_collections WHERE id = NEW.id;
  IF NOT FOUND OR c.status = 'rejected' THEN RETURN NULL; END IF;
  SELECT * INTO t FROM public.incoming_bank_transfers WHERE id = c.incoming_transfer_id;
  IF (c.status='requested' AND t.status<>'collection_requested') OR (c.status='collected' AND t.status<>'collected') THEN
    RAISE EXCEPTION 'collection and transfer states disagree' USING ERRCODE='check_violation', CONSTRAINT='receivable_collections_source_state_chk';
  END IF;
  PERFORM pg_advisory_xact_lock(hashtextextended('ledger.payee:' || c.payee_id::text,0));
  SELECT coalesce(sum(amount),0) INTO debt FROM public.ledger_entries WHERE order_line_id=c.order_line_id AND account_code='payee_receivable';
  SELECT coalesce(sum(amount),0) INTO reserved FROM public.receivable_collections WHERE order_line_id=c.order_line_id AND status='requested';
  IF debt < reserved THEN
    RAISE EXCEPTION 'collections exceed outstanding debt' USING ERRCODE='check_violation', CONSTRAINT='receivable_collections_limit_chk';
  END IF;
  IF c.status='collected' AND NOT EXISTS (
    SELECT 1 FROM public.ledger_transactions x WHERE x.id=c.ledger_transaction_id AND x.kind='adjustment' AND x.ref_type='manual' AND x.ref_id=c.id
      AND (SELECT count(*) FROM public.ledger_entries e WHERE e.transaction_id=x.id)=2
      AND EXISTS (SELECT 1 FROM public.ledger_entries e WHERE e.transaction_id=x.id AND e.account_code='koro_bank' AND e.amount=c.amount AND e.order_line_id=c.order_line_id AND e.payee_id IS NULL)
      AND EXISTS (SELECT 1 FROM public.ledger_entries e WHERE e.transaction_id=x.id AND e.account_code='payee_receivable' AND e.amount=-c.amount AND e.order_line_id=c.order_line_id AND e.payee_id IS NULL)) THEN
    RAISE EXCEPTION 'collection ledger does not match its receipt' USING ERRCODE='check_violation', CONSTRAINT='receivable_collections_ledger_chk';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER receivable_collections_consistent AFTER INSERT OR UPDATE ON receivable_collections
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.receivable_collections_check();
CREATE FUNCTION public.incoming_transfer_check_collection() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE current_status text;
BEGIN
 SELECT status INTO current_status FROM public.incoming_bank_transfers WHERE id=NEW.id;
 IF current_status IN ('collection_requested','collected') AND NOT EXISTS (
   SELECT 1 FROM public.receivable_collections c WHERE c.incoming_transfer_id=NEW.id
    AND c.status=CASE current_status WHEN 'collection_requested' THEN 'requested' ELSE 'collected' END) THEN
   RAISE EXCEPTION 'transfer has no matching collection' USING ERRCODE='check_violation', CONSTRAINT='receivable_collections_source_state_chk';
 END IF;
 IF current_status NOT IN ('collection_requested','collected') AND EXISTS (
   SELECT 1 FROM public.receivable_collections c WHERE c.incoming_transfer_id=NEW.id AND c.status IN ('requested','collected')) THEN
   RAISE EXCEPTION 'claimed transfer cannot fund another flow' USING ERRCODE='check_violation', CONSTRAINT='receivable_collections_source_state_chk';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER incoming_bank_transfers_collection_consistent AFTER UPDATE ON incoming_bank_transfers
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.incoming_transfer_check_collection();
GRANT SELECT,INSERT,UPDATE ON receivable_collections TO app_rw;
UPDATE public.retention_policies SET target = 'orders, order_lines, payments, payment_splits, refunds, incoming_bank_transfers, ledger_transactions, ledger_entries, payouts, payout_items, invoices, invoice_lines, legal_parties, legal_party_documents, payees, payee_bank_accounts, files:financial, receivable_collections'
 WHERE code = 'financial_records';

-- Collection evidence ages with its entire financial aggregate.
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
     AND NOT EXISTS (SELECT 1 FROM public.receivable_collections c WHERE c.order_id = p_order
                       AND (c.status = 'requested' OR greatest(c.created_at,c.updated_at) >= p_cutoff))
     AND NOT EXISTS (SELECT 1 FROM public.receivable_collections c JOIN public.incoming_bank_transfers t ON t.id=c.incoming_transfer_id
                      WHERE c.order_id=p_order AND (greatest(t.created_at,t.updated_at) >= p_cutoff
                        OR t.status='collection_requested'))
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
     AND NOT EXISTS (SELECT 1 FROM public.order_lines l WHERE l.order_id=p_order
                      AND (SELECT coalesce(sum(e.amount),0) FROM public.ledger_entries e
                           WHERE e.order_line_id=l.id AND e.account_code='payee_receivable') <> 0)
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
  v_collection_transfers uuid[];
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
         AND (EXISTS (SELECT 1 FROM public.receivable_collections c JOIN public.incoming_bank_transfers t ON t.id=c.incoming_transfer_id
                        WHERE c.order_id=l.order_id AND (
                          EXISTS (SELECT 1 FROM public.receivable_collections other WHERE other.incoming_transfer_id=t.id AND other.order_id<>ALL(v_orders))
                          OR (t.matched_order_id IS NOT NULL AND t.matched_order_id<>ALL(v_orders))))
           OR EXISTS (SELECT 1 FROM public.incoming_bank_transfers t JOIN public.receivable_collections c ON c.incoming_transfer_id=t.id
                       WHERE t.matched_order_id=l.order_id AND c.order_id<>ALL(v_orders))
           OR EXISTS (SELECT 1 FROM public.payout_items i
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
        SELECT other.order_id FROM public.receivable_collections c
          JOIN public.receivable_collections other ON other.incoming_transfer_id=c.incoming_transfer_id
         WHERE c.order_id=ANY(v_sel) AND other.order_id<>ALL(v_sel)
        UNION
        SELECT t.matched_order_id FROM public.receivable_collections c JOIN public.incoming_bank_transfers t ON t.id=c.incoming_transfer_id
         WHERE c.order_id=ANY(v_sel) AND t.matched_order_id IS NOT NULL AND t.matched_order_id<>ALL(v_sel)
        UNION
        SELECT c.order_id FROM public.incoming_bank_transfers t JOIN public.receivable_collections c ON c.incoming_transfer_id=t.id
         WHERE t.matched_order_id=ANY(v_sel) AND c.order_id<>ALL(v_sel)
        UNION
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

  v_collection_transfers := ARRAY(SELECT DISTINCT c.incoming_transfer_id FROM public.receivable_collections c
    WHERE c.order_id=ANY(v_sel) AND NOT EXISTS (SELECT 1 FROM public.receivable_collections other
      WHERE other.incoming_transfer_id=c.incoming_transfer_id AND other.order_id<>ALL(v_sel))
    AND NOT EXISTS (SELECT 1 FROM public.incoming_bank_transfers t WHERE t.id=c.incoming_transfer_id AND t.matched_order_id IS NOT NULL));
  IF p_dry_run THEN
    v_n := cardinality(v_sel) + cardinality(v_lines) + cardinality(v_payouts) + cardinality(v_txns) + cardinality(v_inv)
         + (SELECT count(*) FROM public.payments p WHERE p.order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.payment_splits s WHERE s.order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.refunds r WHERE r.order_id = ANY (v_sel))
         + (SELECT count(*) FROM public.receivable_collections c WHERE c.order_id = ANY (v_sel))
         + cardinality(v_collection_transfers)
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
    DELETE FROM public.receivable_collections WHERE order_id = ANY (v_sel);
    GET DIAGNOSTICS v_k = ROW_COUNT; v_n := v_n + v_k;
    DELETE FROM public.incoming_bank_transfers WHERE id = ANY (v_collection_transfers);
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
                    AND NOT EXISTS (SELECT 1 FROM public.receivable_collections c WHERE c.incoming_transfer_id=t.id)
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
COMMIT;
