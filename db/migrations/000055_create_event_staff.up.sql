-- 000055_create_event_staff.up.sql (02 §11.2). Door staff of an event
-- (scanner / manager), added by the club_admin of the organiser
-- (event.staff.manage). Hard-deleted 90 days after the event.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE event_staff (
  event_id    uuid NOT NULL CONSTRAINT event_staff_event_id_fkey REFERENCES events (id),
  user_id     uuid NOT NULL CONSTRAINT event_staff_user_id_fkey REFERENCES users (id),
  org_id      uuid NOT NULL CONSTRAINT event_staff_org_id_fkey REFERENCES organizations (id),
  role        text NOT NULL CONSTRAINT event_staff_role_chk CHECK (role IN ('scanner','manager')),
  invited_by  uuid NOT NULL CONSTRAINT event_staff_invited_by_fkey REFERENCES users (id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (event_id, user_id)
);
CREATE INDEX event_staff_user_idx ON event_staff (user_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON event_staff TO app_rw;
COMMIT;
