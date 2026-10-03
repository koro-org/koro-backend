-- 000115_create_payout_items.up.sql (02 §21.5). The order lines a payout
-- pays, with the amount per line (seller_net_amount minus the line's refund
-- share, platform/money). A line is in at most one payout; when a payout
-- fails or is cancelled its items are deleted (the only hard delete,
-- payout_items_guard) and the lines are payable again.
-- payout_items_sum (deferred constraint trigger on payout_items and
-- payouts): at commit the items of every live payout (not failed /
-- cancelled) sum to payouts.amount, and a 'paid' payout has marked every
-- one of its lines paid out (order_lines.payout_id). Raised as
-- check_violation with CONSTRAINT payout_items_sum / payouts_lines_paid_out.
-- 02 additions (H-20):
--   * 06 H-38: payee_id (new column) with (payout_id, payee_id) → payouts and
--     (order_line_id, payee_id) → order_lines: a payout pays only lines of
--     its own payee. order_lines (payout_id, id) → payout_items: a line is
--     marked paid out only by a payout that contains it.
--   * payout_items_guard: items are added only while the payout is pending
--     approval, only for a fulfilled line not yet paid out and never above
--     the line's seller_net_amount (check_violation, CONSTRAINT
--     payout_items_line_chk); they are deleted only with a failed or
--     cancelled payout (restrict_violation). app_rw: SELECT, INSERT, DELETE.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE payout_items (
  payout_id      uuid NOT NULL,
  order_line_id  uuid NOT NULL,
  payee_id       uuid NOT NULL,                     -- = payouts.payee_id = order_lines.payee_id
  amount         bigint NOT NULL CONSTRAINT payout_items_amount_chk CHECK (amount > 0),
  created_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payout_items_pkey PRIMARY KEY (payout_id, order_line_id),
  CONSTRAINT payout_items_payout_id_payee_id_fkey FOREIGN KEY (payout_id, payee_id) REFERENCES payouts (id, payee_id),
  CONSTRAINT payout_items_order_line_id_payee_id_fkey FOREIGN KEY (order_line_id, payee_id) REFERENCES order_lines (id, payee_id)
);
CREATE UNIQUE INDEX payout_items_line_uq ON payout_items (order_line_id);   -- a line is in one payout at a time

ALTER TABLE order_lines ADD CONSTRAINT order_lines_payout_id_id_fkey
  FOREIGN KEY (payout_id, id) REFERENCES payout_items (payout_id, order_line_id);

-- Items join a pending payout only, for a payable line and within its
-- seller_net_amount; they leave only with a failed or cancelled payout.
CREATE FUNCTION public.payout_items_check() RETURNS trigger
  LANGUAGE plpgsql AS $$
DECLARE
  payout_status text;
  line          record;
BEGIN
  IF TG_OP = 'DELETE' THEN
    SELECT p.status INTO payout_status FROM payouts p WHERE p.id = OLD.payout_id;
    IF payout_status IS NOT NULL AND payout_status NOT IN ('failed','cancelled') THEN
      RAISE EXCEPTION 'DELETE on %.% is not allowed: payout % is %', TG_TABLE_SCHEMA, TG_TABLE_NAME, OLD.payout_id, payout_status
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN OLD;
  END IF;
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION 'UPDATE on %.% is not allowed: an item is replaced by delete + insert', TG_TABLE_SCHEMA, TG_TABLE_NAME
      USING ERRCODE = 'restrict_violation';
  END IF;
  SELECT p.status INTO payout_status FROM payouts p WHERE p.id = NEW.payout_id;
  SELECT l.fulfillment_status, l.payout_id, l.seller_net_amount INTO line FROM order_lines l WHERE l.id = NEW.order_line_id;
  IF payout_status IS DISTINCT FROM 'pending_approval' OR line.fulfillment_status IS DISTINCT FROM 'fulfilled'
     OR line.payout_id IS NOT NULL OR NEW.amount > line.seller_net_amount THEN
    RAISE EXCEPTION 'payout %: line % cannot be added (payout %, line %, paid out %, amount % of %)', NEW.payout_id, NEW.order_line_id,
      payout_status, line.fulfillment_status, line.payout_id IS NOT NULL, NEW.amount, line.seller_net_amount
      USING ERRCODE = 'check_violation', CONSTRAINT = 'payout_items_line_chk';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payout_items_guard BEFORE INSERT OR UPDATE OR DELETE ON payout_items
  FOR EACH ROW EXECUTE FUNCTION public.payout_items_check();

-- TG_ARGV[0] names the column of the trigger row that holds the payout id.
CREATE FUNCTION public.payouts_check_items() RETURNS trigger
  LANGUAGE plpgsql AS $$
DECLARE
  payout_ref  uuid;
  p           record;
  item_count  bigint;
  item_sum    numeric;
  unmarked    bigint;
BEGIN
  IF TG_OP = 'DELETE' THEN
    payout_ref := (to_jsonb(OLD) ->> TG_ARGV[0])::uuid;
  ELSE
    payout_ref := (to_jsonb(NEW) ->> TG_ARGV[0])::uuid;
  END IF;
  SELECT po.id, po.status, po.amount INTO p FROM payouts po WHERE po.id = payout_ref;
  IF NOT FOUND OR p.status IN ('failed','cancelled') THEN
    RETURN NULL;
  END IF;
  SELECT count(*), coalesce(sum(i.amount), 0),
         count(*) FILTER (WHERE l.payout_id IS DISTINCT FROM i.payout_id)
    INTO item_count, item_sum, unmarked
    FROM payout_items i JOIN order_lines l ON l.id = i.order_line_id
   WHERE i.payout_id = payout_ref;
  IF item_count = 0 OR item_sum <> p.amount THEN
    RAISE EXCEPTION 'payout % (%): % item(s) sum to %, the payout is %', payout_ref, p.status, item_count, item_sum, p.amount
      USING ERRCODE = 'check_violation', CONSTRAINT = 'payout_items_sum';
  END IF;
  IF p.status = 'paid' AND unmarked > 0 THEN
    RAISE EXCEPTION 'payout % is paid but % of its lines are not marked paid out', payout_ref, unmarked
      USING ERRCODE = 'check_violation', CONSTRAINT = 'payouts_lines_paid_out';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER payout_items_sum AFTER INSERT OR UPDATE OR DELETE ON payout_items
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.payouts_check_items('payout_id');
CREATE CONSTRAINT TRIGGER payout_items_sum AFTER INSERT OR UPDATE ON payouts
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.payouts_check_items('id');
GRANT SELECT, INSERT, DELETE ON payout_items TO app_rw;
COMMIT;
