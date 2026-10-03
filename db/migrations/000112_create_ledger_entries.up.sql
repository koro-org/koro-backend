-- 000112_create_ledger_entries.up.sql (02 §21.3). The entries of the
-- double-entry ledger (append-only): amount in kuruş, debit +, credit −.
-- Balance (T-44): the deferred constraint trigger ledger_txn_balanced runs
-- at commit for every new entry and every new transaction header: a
-- transaction has at least two entries and they sum to zero; an entry on a
-- per_payee account names a payee and an entry on any other account does
-- not; entries go only to active accounts. Errors are raised as
-- check_violation with CONSTRAINT ledger_txn_balanced (balance) or
-- ledger_entries_account_chk (payee / active rule), which platform/db
-- Translate maps to problem.ErrValidation at commit (WithTx).
-- Model A templates (02 §21.3): payment_received, order_allocated,
-- payout_paid, refund_completed. Two layers (02 §0.5): app_rw SELECT/INSERT,
-- koro_forbid_mutation for UPDATE/DELETE/TRUNCATE.
-- 02 additions (H-20):
--   * the payee rule holds in both directions (02: only per_payee ⇒ payee);
--   * 06 H-38: (order_line_id, payee_id) → order_lines (id, payee_id): an
--     entry that names both names the line's own payee.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE ledger_entries (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  transaction_id  uuid NOT NULL CONSTRAINT ledger_entries_transaction_id_fkey REFERENCES ledger_transactions (id),
  account_code    text NOT NULL CONSTRAINT ledger_entries_account_code_fkey REFERENCES ledger_accounts (code),
  payee_id        uuid CONSTRAINT ledger_entries_payee_id_fkey REFERENCES payees (id),
  order_line_id   uuid CONSTRAINT ledger_entries_order_line_id_fkey REFERENCES order_lines (id),
  amount          bigint NOT NULL CONSTRAINT ledger_entries_amount_chk CHECK (amount <> 0),   -- debit +, credit −
  currency        char(3) NOT NULL DEFAULT 'TRY' CONSTRAINT ledger_entries_currency_chk CHECK (currency = 'TRY'),
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ledger_entries_order_line_id_payee_id_fkey FOREIGN KEY (order_line_id, payee_id) REFERENCES order_lines (id, payee_id)
);
CREATE INDEX ledger_entries_txn_idx   ON ledger_entries (transaction_id);
CREATE INDEX ledger_entries_payee_idx ON ledger_entries (account_code, payee_id) WHERE payee_id IS NOT NULL;
CREATE INDEX ledger_entries_line_idx  ON ledger_entries (order_line_id) WHERE order_line_id IS NOT NULL;
CREATE TRIGGER ledger_entries_immutable BEFORE UPDATE OR DELETE ON ledger_entries
  FOR EACH ROW EXECUTE FUNCTION public.koro_forbid_mutation();
CREATE TRIGGER ledger_entries_no_truncate BEFORE TRUNCATE ON ledger_entries
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();

-- TG_ARGV[0] names the column of the trigger row that holds the ledger
-- transaction id ('id' on ledger_transactions, 'transaction_id' on entries).
CREATE FUNCTION public.ledger_check_balanced() RETURNS trigger
  LANGUAGE plpgsql AS $$
DECLARE
  txn          uuid := (to_jsonb(NEW) ->> TG_ARGV[0])::uuid;
  entry_count  bigint;
  total        numeric;
  bad_account  bigint;
BEGIN
  SELECT count(*), coalesce(sum(e.amount), 0),
         count(*) FILTER (WHERE a.per_payee <> (e.payee_id IS NOT NULL) OR NOT a.active)
    INTO entry_count, total, bad_account
    FROM ledger_entries e JOIN ledger_accounts a ON a.code = e.account_code
   WHERE e.transaction_id = txn;
  IF entry_count < 2 OR total <> 0 THEN
    RAISE EXCEPTION 'ledger transaction % is not balanced: % entries, sum %', txn, entry_count, total
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_txn_balanced';
  END IF;
  IF bad_account > 0 THEN
    RAISE EXCEPTION 'ledger transaction % has % entries that break the payee rule or use an inactive account', txn, bad_account
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_account_chk';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER ledger_txn_balanced AFTER INSERT ON ledger_entries
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.ledger_check_balanced('transaction_id');
CREATE CONSTRAINT TRIGGER ledger_txn_balanced AFTER INSERT ON ledger_transactions
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.ledger_check_balanced('id');
GRANT SELECT, INSERT ON ledger_entries TO app_rw;
COMMIT;
