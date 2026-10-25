-- 000147_iys_netgsm_events.up.sql (BE-110; 02 §4 doc patch).
-- The webhook items of the Netgsm İYS adapter (internal/platform/iys/netgsm):
-- Netgsm pushes İYS's answer to every upload (kind 'result': success with
-- İYS's transaction ID, or failure with its Hxxx code; Submit reads it) and
-- every change a recipient gives İYS itself (kind 'change', an IYS_*
-- source: the change feed consent.iys_status_sync reads after seq). The
-- backup check (consent.iys_netgsm_recheck, every 10 minutes) adds the refusals İYS
-- holds that no delivery brought.
-- Least data (KVKK): the address is resolved to the Koro user on receipt
-- and never stored, nor the citizen's name or Netgsm's error text; user_id
-- is NULL for an address of no Koro user (an anonymous marker, counted).
-- Rows are deleted seven days after receipt by the daily
-- consent.iys_netgsm_purge (the feed's window; retention policy
-- iys_netgsm_event, seeded here with the table: the period can be
-- lengthened, never below the window); de-duplicated by
-- (feed, event_key), a SHA-256 hex digest. No FK on user_id: a row of an
-- erased account goes with the retention.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE iys_netgsm_events (
  seq             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  feed            text NOT NULL CONSTRAINT iys_netgsm_events_feed_chk CHECK (feed ~ '^[a-z0-9][a-z0-9:/._-]{0,99}$'),
  event_key       text NOT NULL CONSTRAINT iys_netgsm_events_event_key_chk CHECK (event_key ~ '^[0-9a-f]{64}$'),
  kind            text NOT NULL CONSTRAINT iys_netgsm_events_kind_chk CHECK (kind IN ('change', 'result')),
  ref             text CONSTRAINT iys_netgsm_events_ref_chk CHECK (ref ~ '^[\x21-\x7e]{1,64}$'),
  submit_id       text CONSTRAINT iys_netgsm_events_submit_id_chk CHECK (submit_id ~ '^[0-9A-Za-z-]{1,128}$'),
  user_id         uuid,
  type            text NOT NULL CONSTRAINT iys_netgsm_events_type_chk CHECK (type IN ('EPOSTA', 'MESAJ', 'ARAMA')),
  status          text NOT NULL CONSTRAINT iys_netgsm_events_status_chk CHECK (status IN ('ONAY', 'RET')),
  source          text NOT NULL DEFAULT '' CONSTRAINT iys_netgsm_events_source_chk CHECK (source ~ '^([A-Z][A-Z0-9_]{0,31})?$'),
  consent_at      timestamptz,
  recorded_at     timestamptz,
  result          text CONSTRAINT iys_netgsm_events_result_chk CHECK (result IN ('success', 'failure')),
  err_code        text CONSTRAINT iys_netgsm_events_err_code_chk CHECK (err_code ~ '^H[0-9]{1,5}$'),
  transaction_id  text CONSTRAINT iys_netgsm_events_transaction_id_chk CHECK (transaction_id ~ '^[0-9A-Za-z-]{1,128}$'),
  received_at     timestamptz NOT NULL,
  CONSTRAINT iys_netgsm_events_feed_event_key_uq UNIQUE (feed, event_key),
  -- An answer carries its result; a change carries none (İYS took it).
  CONSTRAINT iys_netgsm_events_result_kind_chk CHECK (kind <> 'result' OR result IS NOT NULL),
  CONSTRAINT iys_netgsm_events_change_kind_chk CHECK (kind <> 'change' OR (result IS NULL AND err_code IS NULL AND ref IS NULL)),
  -- A change has a date to judge it by.
  CONSTRAINT iys_netgsm_events_change_date_chk CHECK (kind <> 'change' OR consent_at IS NOT NULL OR recorded_at IS NOT NULL)
);
CREATE INDEX iys_netgsm_events_changes_idx ON iys_netgsm_events (feed, seq) WHERE kind = 'change';
CREATE INDEX iys_netgsm_events_ref_idx ON iys_netgsm_events (feed, ref, seq) WHERE kind = 'result' AND ref IS NOT NULL;
CREATE INDEX iys_netgsm_events_result_user_idx ON iys_netgsm_events (feed, user_id, type, status, consent_at) WHERE kind = 'result';
CREATE INDEX iys_netgsm_events_received_at_idx ON iys_netgsm_events (received_at);
GRANT SELECT, INSERT, DELETE ON iys_netgsm_events TO app_rw;
-- The retention of the table (02 §24.2; operational like outbox_published:
-- its job purges without waiting for the legal approval; unapproved like
-- every seeded row, T-35).
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('iys_netgsm_event', 'consent', 'iys_netgsm_events', '7 days', 'created_at', 'hard_delete', 'Operasyonel (veri minimizasyonu)', true,
   'Netgsm İYS webhook kalemleri (yalnız kullanıcı kimliği; adres yok) alındıktan 7 gün sonra silinir; değişiklik akışının penceresi bu süredir.')
ON CONFLICT (code) DO NOTHING;
COMMIT;
