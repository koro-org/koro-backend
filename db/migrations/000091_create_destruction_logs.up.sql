-- 000091_create_destruction_logs.up.sql (02 §16.3). Append-only record of
-- every destruction (KVKK imha kaydı): one row per policy and run, account
-- erasures included (policy_code 'dsr_erasure', one row per module). No
-- personal data (counts and errors only). Two layers (02 §0.5): app_rw has
-- SELECT/INSERT only and koro_forbid_mutation blocks UPDATE/DELETE/TRUNCATE
-- for everyone. Kept 3 years (destruction_log).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE destruction_logs (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  policy_code     text NOT NULL CONSTRAINT destruction_logs_policy_code_fkey REFERENCES retention_policies (code),
  run_id          uuid NOT NULL,                       -- all rows of one nightly run
  module          text NOT NULL,
  target          text NOT NULL,
  method          text NOT NULL CONSTRAINT destruction_logs_method_chk CHECK (method IN ('hard_delete','anonymize','null_columns','file_delete')),
  record_count    integer NOT NULL CONSTRAINT destruction_logs_record_count_chk CHECK (record_count >= 0),
  period_cutoff   timestamptz NOT NULL,                -- records older than this
  started_at      timestamptz NOT NULL,
  finished_at     timestamptz NOT NULL,
  details         jsonb NOT NULL DEFAULT '{}',          -- no personal data (counts / errors only)
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX destruction_logs_policy_idx ON destruction_logs (policy_code, created_at DESC);
CREATE TRIGGER destruction_logs_immutable BEFORE UPDATE OR DELETE ON destruction_logs
  FOR EACH ROW EXECUTE FUNCTION public.koro_forbid_mutation();
CREATE TRIGGER destruction_logs_no_truncate BEFORE TRUNCATE ON destruction_logs
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();
GRANT SELECT, INSERT ON destruction_logs TO app_rw;
COMMIT;
