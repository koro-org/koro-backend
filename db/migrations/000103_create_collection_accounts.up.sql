-- 000103_create_collection_accounts.up.sql (02 §20.1; 03 §6.1). The Koro
-- bank accounts that buyers pay into (MVP: bank transfer to the Koro IBAN).
-- Four eyes: a finance admin creates the account, another admin decides
-- (approve or reject, 04 adminApproveCollectionAccount); approved_by holds
-- the deciding admin. At most one active default account; an IBAN is open
-- (pending or active) once. Soft delete ('retired'); app_rw has no DELETE.
-- 02 fixes and additions (H-20):
--   * 06 H-37: `status = 'rejected' OR ((status IN ('active','retired')) =
--     (approved_by … AND approved_at …))` let a pending account carry half a
--     decision; now approved_by and approved_at are each set exactly when
--     the account is decided (active, retired, rejected), retired_at exactly
--     when it is retired.
--   * the account belongs to the Koro legal party: (legal_party_id,
--     party_kind = 'platform') → legal_parties (id, kind).
--   * collection_accounts_frozen: the IBAN, the holder, the party and the
--     creator never change (a new IBAN is a new account under four eyes).
--   * length CHECKs on holder_name and bank_name (as payee_bank_accounts).
--   * koro_initial_status(): a new account starts in 'pending_approval'.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE collection_accounts (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  label            text NOT NULL CONSTRAINT collection_accounts_label_chk CHECK (char_length(label) <= 100),
  legal_party_id   uuid NOT NULL,                                     -- the platform party
  party_kind       text NOT NULL DEFAULT 'platform' CONSTRAINT collection_accounts_party_kind_chk CHECK (party_kind = 'platform'),
  iban             text NOT NULL CONSTRAINT collection_accounts_iban_chk CHECK (public.koro_iban_tr_valid(iban)),
  holder_name      text COLLATE "tr" NOT NULL CONSTRAINT collection_accounts_holder_name_chk CHECK (char_length(holder_name) BETWEEN 2 AND 300),
  bank_name        text NOT NULL CONSTRAINT collection_accounts_bank_name_chk CHECK (char_length(bank_name) BETWEEN 2 AND 120),
  status           text NOT NULL DEFAULT 'pending_approval' CONSTRAINT collection_accounts_status_chk CHECK (status IN ('pending_approval','active','rejected','retired')),
  is_default       boolean NOT NULL DEFAULT false,
  created_by       uuid NOT NULL CONSTRAINT collection_accounts_created_by_fkey REFERENCES users (id),
  approved_by      uuid CONSTRAINT collection_accounts_approved_by_fkey REFERENCES users (id),   -- the deciding admin
  approved_at      timestamptz,
  retired_at       timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT collection_accounts_legal_party_id_party_kind_fkey FOREIGN KEY (legal_party_id, party_kind) REFERENCES legal_parties (id, kind),
  CONSTRAINT collection_accounts_four_eyes_chk CHECK (approved_by IS NULL OR approved_by <> created_by),
  CONSTRAINT collection_accounts_decided_chk CHECK ((status = 'pending_approval') = (approved_by IS NULL)
                                                AND (status = 'pending_approval') = (approved_at IS NULL)),
  CONSTRAINT collection_accounts_retired_chk CHECK ((status = 'retired') = (retired_at IS NOT NULL)),
  CONSTRAINT collection_accounts_default_chk CHECK (NOT is_default OR status = 'active')
);
CREATE UNIQUE INDEX collection_accounts_default_uq ON collection_accounts ((true)) WHERE is_default AND status = 'active';
CREATE UNIQUE INDEX collection_accounts_iban_uq ON collection_accounts (iban) WHERE status IN ('pending_approval','active');
CREATE TRIGGER collection_accounts_touch_updated_at BEFORE UPDATE ON collection_accounts
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER collection_accounts_initial_status BEFORE INSERT ON collection_accounts
  FOR EACH ROW EXECUTE FUNCTION public.koro_initial_status('pending_approval');
CREATE TRIGGER collection_accounts_frozen BEFORE UPDATE ON collection_accounts
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'legal_party_id', 'party_kind', 'iban', 'holder_name', 'bank_name', 'created_by');
GRANT SELECT, INSERT, UPDATE ON collection_accounts TO app_rw;
COMMIT;
