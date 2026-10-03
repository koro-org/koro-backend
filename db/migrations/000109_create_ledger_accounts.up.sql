-- 000109_create_ledger_accounts.up.sql (02 §21.1). The chart of accounts of
-- the double-entry ledger. per_payee accounts (seller_payable) carry a payee
-- on every entry. The accounts are seeded by 000110; the application only
-- reads them (no account endpoint), so app_rw has SELECT only.
-- 02 additions (H-20): name_tr is COLLATE "tr" with a length CHECK.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE ledger_accounts (
  code        text PRIMARY KEY CONSTRAINT ledger_accounts_code_chk CHECK (code ~ '^[a-z_]{3,40}$'),
  name_tr     text COLLATE "tr" NOT NULL CONSTRAINT ledger_accounts_name_tr_chk CHECK (char_length(name_tr) BETWEEN 2 AND 120),
  kind        text NOT NULL CONSTRAINT ledger_accounts_kind_chk CHECK (kind IN ('asset','liability','revenue','expense')),
  per_payee   boolean NOT NULL DEFAULT false,      -- every entry names a payee
  active      boolean NOT NULL DEFAULT true,
  created_at  timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT ON ledger_accounts TO app_rw;
COMMIT;
