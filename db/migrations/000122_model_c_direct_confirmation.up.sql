-- 000122_model_c_direct_confirmation.up
-- BE-066: keep the direct-payment evidence state under the order lock.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE orders ADD COLUMN direct_confirmed_at timestamptz;
-- Only actual captured confirmations are evidence. Unconfirmed historical
-- orders are deliberately not assigned an invented confirmation time.
UPDATE orders o SET direct_confirmed_at = p.captured_at
FROM (SELECT order_id, min(captured_at) AS captured_at FROM payments
      WHERE provider = 'direct_confirmed' AND status = 'captured'
        AND captured_at IS NOT NULL AND evidence_file_id IS NOT NULL
      GROUP BY order_id) p
WHERE o.id = p.order_id AND o.model_code = 'C';
ALTER TABLE orders ADD CONSTRAINT orders_direct_confirmed_model_chk
  CHECK (direct_confirmed_at IS NULL OR model_code = 'C');
CREATE FUNCTION koro_orders_direct_confirmation_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.direct_confirmed_at IS NOT NULL AND NEW.direct_confirmed_at IS DISTINCT FROM OLD.direct_confirmed_at THEN
    RAISE EXCEPTION 'direct payment confirmation is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.direct_confirmed_at IS NULL AND NEW.direct_confirmed_at IS NOT NULL AND OLD.status <> 'awaiting_payment' THEN
    RAISE EXCEPTION 'only an unpaid order may be confirmed' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER orders_direct_confirmation_guard BEFORE UPDATE ON orders
  FOR EACH ROW EXECUTE FUNCTION koro_orders_direct_confirmation_guard();
COMMIT;
