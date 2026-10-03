-- 000093_create_subprocessors.up.sql (02 §16.5). Inventory of the
-- sub-processors (alt işleyen): where personal data goes and on which
-- transfer basis. A foreign processor is never 'domestic'. Retired by
-- active = false, never deleted (no DELETE for app_rw).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE subprocessors (
  id                     uuid PRIMARY KEY DEFAULT uuidv7(),
  name                   text COLLATE "tr" NOT NULL CONSTRAINT subprocessors_name_uq UNIQUE,
  service                text NOT NULL,
  data_categories        text[] NOT NULL,
  region                 text NOT NULL,
  country_code           char(2) NOT NULL CONSTRAINT subprocessors_country_code_chk CHECK (country_code ~ '^[A-Z]{2}$'),
  transfer_mechanism     text NOT NULL CONSTRAINT subprocessors_transfer_mechanism_chk CHECK (transfer_mechanism IN ('domestic','standard_contract','adequacy',
                            'explicit_consent','undecided')),
  contract_signed_at     date,
  authority_notified_at  date,
  active                 boolean NOT NULL DEFAULT true,
  notes                  text,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT subprocessors_domestic_chk CHECK (country_code = 'TR' OR transfer_mechanism <> 'domestic')
);
CREATE TRIGGER subprocessors_touch_updated_at BEFORE UPDATE ON subprocessors
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON subprocessors TO app_rw;
COMMIT;
