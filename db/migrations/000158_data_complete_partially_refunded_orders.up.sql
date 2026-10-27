-- 000158_data_complete_partially_refunded_orders.up.sql (BE-135; 00 T-382). A
-- refund of part of a paid order moved it to partially_refunded, and the
-- order never left that state: orders.progressStatus kept it while the other
-- lines were delivered, so the order never completed and Status.Open kept
-- counting it (its buyer could not be closed, hooks.openCommerce; its units
-- stayed in the buyer's per_buyer_limit count). The code now completes an
-- order once every line is final, with at least one fulfilled, as for any
-- paid order: fulfilled, cancelled (a dropped line included) or its service
-- refunded in full through Koro (not on model C, where Koro held only its
-- fee; T-309: such a line is never delivered). The refunds stay in
-- refunded_amount, the order's and its lines'.
-- This one-off correction applies the same rule to the orders stuck before
-- it: only status and completed_at change — completed_at is kept when the
-- order had completed before a refund moved it back, the correction time
-- otherwise. No money record (refunds, payments, ledger, invoices, payouts)
-- and no line is touched. Idempotent: a second run finds nothing to change.
BEGIN;
SET LOCAL lock_timeout = '5s';
UPDATE orders o SET status = 'completed', completed_at = coalesce(o.completed_at, now())
 WHERE o.status = 'partially_refunded'
   AND EXISTS (SELECT 1 FROM order_lines l WHERE l.order_id = o.id AND l.fulfillment_status = 'fulfilled')
   AND NOT EXISTS (SELECT 1 FROM order_lines l
                    WHERE l.order_id = o.id AND l.fulfillment_status NOT IN ('fulfilled', 'cancelled')
                      AND NOT (o.collection_method <> 'direct_to_payee' AND l.refunded_amount > 0
                               AND l.refunded_amount >= l.payable_amount));
COMMIT;
