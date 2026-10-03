-- 000111_create_ledger_transactions.up.sql (02 §21.2). The header of a
-- balanced ledger transaction (append-only). One transaction per (kind,
-- ref_type, ref_id): posting the same business event twice is a unique
-- violation (idempotent posting). A correction is a new, reversing
-- transaction. Two layers (02 §0.5): app_rw has SELECT/INSERT only and
-- koro_forbid_mutation blocks UPDATE/DELETE/TRUNCATE for everyone. A
-- transaction without balanced entries fails at commit
-- (ledger_txn_balanced, 000112).
-- 02 additions (H-20): ledger_transactions_occurred_idx (journal listing) and
-- ledger_transactions_ref_idx (lookup by reference).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE ledger_transactions (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  kind         text NOT NULL CONSTRAINT ledger_transactions_kind_chk CHECK (kind IN ('payment_received','order_allocated','payout_paid','refund_completed',
                 'fee','adjustment')),
  ref_type     text NOT NULL CONSTRAINT ledger_transactions_ref_type_chk CHECK (ref_type IN ('payment','order','payout','refund','manual')),
  ref_id       uuid NOT NULL,
  occurred_at  timestamptz NOT NULL,
  memo         text CONSTRAINT ledger_transactions_memo_chk CHECK (char_length(memo) <= 500),
  created_by   uuid CONSTRAINT ledger_transactions_created_by_fkey REFERENCES users (id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ledger_transactions_ref_uq UNIQUE (kind, ref_type, ref_id)
);
CREATE INDEX ledger_transactions_occurred_idx ON ledger_transactions (occurred_at DESC, id DESC);
CREATE INDEX ledger_transactions_ref_idx      ON ledger_transactions (ref_type, ref_id);
CREATE TRIGGER ledger_transactions_immutable BEFORE UPDATE OR DELETE ON ledger_transactions
  FOR EACH ROW EXECUTE FUNCTION public.koro_forbid_mutation();
CREATE TRIGGER ledger_transactions_no_truncate BEFORE TRUNCATE ON ledger_transactions
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();
GRANT SELECT, INSERT ON ledger_transactions TO app_rw;
COMMIT;
