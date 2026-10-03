-- 000097_create_payee_bank_accounts.up.sql (02 §17.4; 03 §6.1). The bank
-- account (IBAN) of a payee, approved by four eyes: a payout goes only to an
-- 'approved' account, and an account change is a new row (the old approved
-- row becomes 'superseded' in the approving transaction; one approved and
-- one pending row per payee).
--   org submission:   pending_verification → admin A verify → pending_approval
--                     → admin B (B ≠ A, B ≠ submitter) approve → approved
--   admin submission: pending_approval (the submitter is the first eye)
--                     → admin B (B ≠ submitter) approve → approved
-- The IBAN is checked by koro_iban_tr_valid (000002; the same vectors as
-- internal/platform/iban, T-42). Never deleted (financial_records).
-- 02 fixes and additions (H-20):
--   * 06 H-37: the state CHECKs `(status IN ('approved','superseded')) =
--     (approved_by … AND approved_at …)`, `(status = 'rejected') =
--     (rejected_by … AND rejection_reason …)`, `(status = 'revoked') =
--     (revoked_at … AND revoke_reason …)` let a row outside the state carry
--     half of its columns. Every column is now tied to its state on its own:
--     approved_by/at belong to approved, superseded and revoked (only an
--     approved account is revoked; a pending one is rejected), the
--     rejected_*, superseded_* and revoked_* columns to their state;
--     verified_by/at are set together and never on pending_verification; an
--     admin submission never waits for verification and has no separate
--     first eye.
--   * 06 H-38: (payee_id, org_id) → payees and (superseded_by_id, payee_id) →
--     payee_bank_accounts: the account and its successor belong to one payee
--     and its org.
--   * koro_initial_status(): a new account starts pending (verification or
--     approval), never approved.
--   * koro_freeze_columns(): the IBAN, the holder, the payee and the
--     submitter of a row never change (a change is a new row that goes
--     through four eyes again); a changed IBAN under an existing
--     verification or approval would bypass both eyes.
BEGIN;
SET LOCAL lock_timeout = '5s';

-- Generic guard for money tables: BEFORE UPDATE row trigger with
-- TG_ARGV[0] = the guard column and TG_ARGV[1..] = the frozen columns. Once
-- the guard column of the old row is not NULL (use 'id' for "always"), the
-- frozen columns must keep their value. Unknown column names fail loudly.
CREATE FUNCTION public.koro_freeze_columns() RETURNS trigger
  LANGUAGE plpgsql AS $$
DECLARE
  old_row jsonb := to_jsonb(OLD);
  new_row jsonb := to_jsonb(NEW);
  col     text;
BEGIN
  IF TG_NARGS < 2 OR NOT (old_row ? TG_ARGV[0]) THEN
    RAISE EXCEPTION 'koro_freeze_columns on %.%: bad guard column %', TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_ARGV[0];
  END IF;
  IF old_row ->> TG_ARGV[0] IS NULL THEN
    RETURN NEW;
  END IF;
  FOR i IN 1 .. TG_NARGS - 1 LOOP
    col := TG_ARGV[i];
    IF NOT (old_row ? col) THEN
      RAISE EXCEPTION 'koro_freeze_columns on %.%: no column %', TG_TABLE_SCHEMA, TG_TABLE_NAME, col;
    END IF;
    IF (old_row -> col) IS DISTINCT FROM (new_row -> col) THEN
      RAISE EXCEPTION 'UPDATE of %.% is not allowed: % is frozen once % is set', TG_TABLE_SCHEMA, TG_TABLE_NAME, col, TG_ARGV[0]
        USING ERRCODE = 'restrict_violation';
    END IF;
  END LOOP;
  RETURN NEW;
END $$;

-- Four-eyes tables start in their first state: BEFORE INSERT row trigger
-- with TG_ARGV = the allowed initial statuses. A row written straight into
-- an approved state would claim both eyes in one statement.
CREATE FUNCTION public.koro_initial_status() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  IF NOT (NEW.status = ANY (TG_ARGV)) THEN
    RAISE EXCEPTION 'a new %.% row starts in % (not %)', TG_TABLE_SCHEMA, TG_TABLE_NAME, array_to_string(TG_ARGV, ' or '), NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = TG_TABLE_NAME || '_initial_status_chk';
  END IF;
  RETURN NEW;
END $$;

CREATE TABLE payee_bank_accounts (
  id                  uuid PRIMARY KEY DEFAULT uuidv7(),
  payee_id            uuid NOT NULL,
  org_id              uuid NOT NULL,                                       -- = payees.org_id (composite FK)
  iban                text NOT NULL CONSTRAINT payee_bank_accounts_iban_chk CHECK (public.koro_iban_tr_valid(iban)),   -- normalised: 'TR' + 24 digits
  holder_name         text COLLATE "tr" NOT NULL CONSTRAINT payee_bank_accounts_holder_name_chk CHECK (char_length(holder_name) BETWEEN 2 AND 300),
  bank_code           text GENERATED ALWAYS AS (substr(iban, 5, 5)) STORED,
  bank_name           text CONSTRAINT payee_bank_accounts_bank_name_chk CHECK (char_length(bank_name) <= 120),
  evidence_file_id    uuid NOT NULL CONSTRAINT payee_bank_accounts_evidence_file_id_fkey REFERENCES files (id),   -- account statement / bank letter
  status              text NOT NULL CONSTRAINT payee_bank_accounts_status_chk CHECK (status IN ('pending_verification','pending_approval','approved',
                        'rejected','superseded','revoked')),
  submitted_by        uuid NOT NULL CONSTRAINT payee_bank_accounts_submitted_by_fkey REFERENCES users (id),
  submitted_by_admin  boolean NOT NULL,       -- submitted by a finance_admin / platform_admin (written by the app, checked in audit)
  submitted_at        timestamptz NOT NULL DEFAULT now(),
  verified_by         uuid CONSTRAINT payee_bank_accounts_verified_by_fkey REFERENCES users (id),   -- first eye (org submissions)
  verified_at         timestamptz,
  approved_by         uuid CONSTRAINT payee_bank_accounts_approved_by_fkey REFERENCES users (id),   -- second eye
  approved_at         timestamptz,
  rejected_by         uuid CONSTRAINT payee_bank_accounts_rejected_by_fkey REFERENCES users (id),
  rejected_at         timestamptz,
  rejection_reason    text CONSTRAINT payee_bank_accounts_rejection_reason_chk CHECK (char_length(rejection_reason) <= 500),
  superseded_at       timestamptz,
  superseded_by_id    uuid,
  revoked_by          uuid CONSTRAINT payee_bank_accounts_revoked_by_fkey REFERENCES users (id),
  revoked_at          timestamptz,
  revoke_reason       text CONSTRAINT payee_bank_accounts_revoke_reason_chk CHECK (char_length(revoke_reason) <= 500),
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payee_bank_accounts_payee_id_org_id_fkey FOREIGN KEY (payee_id, org_id) REFERENCES payees (id, org_id),
  CONSTRAINT payee_bank_accounts_id_payee_uq UNIQUE (id, payee_id),
  CONSTRAINT payee_bank_accounts_superseded_by_id_payee_id_fkey FOREIGN KEY (superseded_by_id, payee_id) REFERENCES payee_bank_accounts (id, payee_id),
  -- four eyes (03 §6.1)
  CONSTRAINT payee_bank_accounts_approver_submitter_chk CHECK (approved_by IS NULL OR approved_by <> submitted_by),
  CONSTRAINT payee_bank_accounts_approver_verifier_chk  CHECK (approved_by IS NULL OR verified_by IS NULL OR approved_by <> verified_by),
  CONSTRAINT payee_bank_accounts_verifier_submitter_chk CHECK (verified_by IS NULL OR verified_by <> submitted_by),
  CONSTRAINT payee_bank_accounts_org_flow_chk   CHECK (submitted_by_admin OR status IN ('pending_verification','rejected') OR verified_by IS NOT NULL),
  CONSTRAINT payee_bank_accounts_admin_flow_chk CHECK (NOT submitted_by_admin OR (status <> 'pending_verification' AND verified_by IS NULL)),
  -- one CHECK per state (06 H-37)
  CONSTRAINT payee_bank_accounts_verified_chk CHECK ((verified_by IS NULL) = (verified_at IS NULL)
                                                  AND (status <> 'pending_verification' OR verified_by IS NULL)),
  CONSTRAINT payee_bank_accounts_approved_chk CHECK ((status IN ('approved','superseded','revoked')) = (approved_by IS NOT NULL)
                                                  AND (status IN ('approved','superseded','revoked')) = (approved_at IS NOT NULL)),
  CONSTRAINT payee_bank_accounts_rejected_chk CHECK ((status = 'rejected') = (rejected_by IS NOT NULL)
                                                  AND (status = 'rejected') = (rejected_at IS NOT NULL)
                                                  AND (status = 'rejected') = (rejection_reason IS NOT NULL)),
  CONSTRAINT payee_bank_accounts_superseded_chk CHECK ((status = 'superseded') = (superseded_at IS NOT NULL)
                                                    AND (status = 'superseded') = (superseded_by_id IS NOT NULL)
                                                    AND (superseded_by_id IS NULL OR superseded_by_id <> id)),
  CONSTRAINT payee_bank_accounts_revoked_chk CHECK ((status = 'revoked') = (revoked_by IS NOT NULL)
                                                 AND (status = 'revoked') = (revoked_at IS NOT NULL)
                                                 AND (status = 'revoked') = (revoke_reason IS NOT NULL))
);
CREATE UNIQUE INDEX payee_bank_accounts_approved_uq ON payee_bank_accounts (payee_id) WHERE status = 'approved';
CREATE UNIQUE INDEX payee_bank_accounts_pending_uq  ON payee_bank_accounts (payee_id)
  WHERE status IN ('pending_verification','pending_approval');
CREATE INDEX payee_bank_accounts_queue_idx ON payee_bank_accounts (submitted_at)
  WHERE status IN ('pending_verification','pending_approval');
CREATE TRIGGER payee_bank_accounts_touch_updated_at BEFORE UPDATE ON payee_bank_accounts
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER payee_bank_accounts_initial_status BEFORE INSERT ON payee_bank_accounts
  FOR EACH ROW EXECUTE FUNCTION public.koro_initial_status('pending_verification', 'pending_approval');
CREATE TRIGGER payee_bank_accounts_frozen BEFORE UPDATE ON payee_bank_accounts
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'payee_id', 'org_id', 'iban', 'holder_name', 'bank_name',
    'evidence_file_id', 'submitted_by', 'submitted_by_admin', 'submitted_at');
GRANT SELECT, INSERT, UPDATE ON payee_bank_accounts TO app_rw;
COMMIT;
