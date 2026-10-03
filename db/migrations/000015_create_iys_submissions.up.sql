-- 000015_create_iys_submissions.up.sql (02 §4.4). One İYS (commercial message
-- registry) submission per consent row that needs one.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE iys_submissions (
  id                  uuid PRIMARY KEY DEFAULT uuidv7(),
  consent_id          uuid NOT NULL CONSTRAINT iys_submissions_consent_id_uq UNIQUE CONSTRAINT iys_submissions_consent_id_fkey REFERENCES consents (id),
  channel             text NOT NULL CONSTRAINT iys_submissions_channel_chk CHECK (channel IN ('email','sms','call')),
  status              text NOT NULL DEFAULT 'pending' CONSTRAINT iys_submissions_status_chk CHECK (status IN ('pending','sent','failed','skipped')),
  iys_transaction_id  text,
  attempts            integer NOT NULL DEFAULT 0 CONSTRAINT iys_submissions_attempts_chk CHECK (attempts >= 0),
  last_error          text CONSTRAINT iys_submissions_last_error_chk CHECK (char_length(last_error) <= 2000),
  sent_at             timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT iys_submissions_sent_chk CHECK (status <> 'sent' OR sent_at IS NOT NULL)
);
CREATE INDEX iys_submissions_pending_idx ON iys_submissions (created_at) WHERE status IN ('pending','failed');
CREATE TRIGGER iys_submissions_touch_updated_at BEFORE UPDATE ON iys_submissions
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON iys_submissions TO app_rw;
COMMIT;
