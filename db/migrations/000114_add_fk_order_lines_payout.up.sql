-- 000114_add_fk_order_lines_payout.up.sql (02 §21.4, §23). The forward FK of
-- order_lines (000102) to the payout that paid the line out. 06 H-38: the
-- FK is (payout_id, payee_id) → payouts (id, payee_id), so a line is only
-- ever paid out by a payout of its own payee (order_lines_payout_line_chk
-- already requires a payee for a paid-out line). order_lines is empty when
-- this runs.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE order_lines ADD CONSTRAINT order_lines_payout_id_payee_id_fkey
  FOREIGN KEY (payout_id, payee_id) REFERENCES payouts (id, payee_id);
COMMIT;
