-- 000092_create_breach_incidents.up.sql (02 §16.4). Personal data breach
-- register: the 72-hour authority deadline, the notifications and the
-- status. No person's name is written into the description (rule). Kept 5
-- years after closing (breach_record).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE breach_incidents (
  id                       uuid PRIMARY KEY DEFAULT uuidv7(),
  title                    text COLLATE "tr" NOT NULL CONSTRAINT breach_incidents_title_chk CHECK (char_length(title) <= 200),
  detected_at              timestamptz NOT NULL,
  authority_deadline_at    timestamptz NOT NULL,          -- detected_at + 72 hours (written by the service)
  controller_notified_at   timestamptz,                   -- notice to the data controller
  authority_notified_at    timestamptz,                   -- notice to the Board (made by the controller; recorded)
  data_categories          text[] NOT NULL DEFAULT '{}',
  affected_estimate        integer CONSTRAINT breach_incidents_affected_estimate_chk CHECK (affected_estimate >= 0),
  description              text NOT NULL CONSTRAINT breach_incidents_description_chk CHECK (char_length(description) <= 10000),
  status                   text NOT NULL DEFAULT 'open' CONSTRAINT breach_incidents_status_chk CHECK (status IN ('open','contained','notified','closed')),
  owner_user_id            uuid NOT NULL CONSTRAINT breach_incidents_owner_user_id_fkey REFERENCES users (id),
  closed_at                timestamptz,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT breach_incidents_closed_chk CHECK ((status = 'closed') = (closed_at IS NOT NULL))
);
CREATE TRIGGER breach_incidents_touch_updated_at BEFORE UPDATE ON breach_incidents
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON breach_incidents TO app_rw;
COMMIT;
