-- 000063_create_event_checkins.up.sql (02 §11.8). A door scan. client_scan_id
-- makes a client retry return the same answer; a registration can have only
-- one valid (not voided) check-in at a time, which the partial unique index
-- event_checkins_one_active_uq guarantees even under concurrent scans.
-- Voiding keeps the row (voided_at / voided_by / void_reason together).
-- Deleted with the registration (ON DELETE CASCADE) on account deletion.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE event_checkins (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  event_id         uuid NOT NULL CONSTRAINT event_checkins_event_id_fkey REFERENCES events (id),
  org_id           uuid NOT NULL CONSTRAINT event_checkins_org_id_fkey REFERENCES organizations (id),
  registration_id  uuid NOT NULL CONSTRAINT event_checkins_registration_id_fkey REFERENCES event_registrations (id) ON DELETE CASCADE,
  ticket_id        uuid NOT NULL CONSTRAINT event_checkins_ticket_id_fkey REFERENCES registration_tickets (id) ON DELETE CASCADE,
  staff_user_id    uuid NOT NULL CONSTRAINT event_checkins_staff_user_id_fkey REFERENCES users (id),
  method           text NOT NULL CONSTRAINT event_checkins_method_chk CHECK (method IN ('qr','manual_code')),
  device_id        text CONSTRAINT event_checkins_device_id_chk CHECK (char_length(device_id) <= 100),
  client_scan_id   uuid NOT NULL CONSTRAINT event_checkins_client_scan_id_uq UNIQUE,   -- client retry = same answer
  scanned_at       timestamptz NOT NULL,
  received_at      timestamptz NOT NULL DEFAULT now(),
  voided_at        timestamptz,
  voided_by        uuid CONSTRAINT event_checkins_voided_by_fkey REFERENCES users (id),
  void_reason      text CONSTRAINT event_checkins_void_reason_chk CHECK (char_length(void_reason) BETWEEN 5 AND 300),
  CONSTRAINT event_checkins_void_actor_chk CHECK ((voided_at IS NULL) = (voided_by IS NULL)),
  CONSTRAINT event_checkins_void_text_chk CHECK ((voided_at IS NULL) = (void_reason IS NULL))
);
CREATE UNIQUE INDEX event_checkins_one_active_uq ON event_checkins (registration_id) WHERE voided_at IS NULL;
CREATE INDEX event_checkins_event_idx ON event_checkins (event_id, scanned_at);
GRANT SELECT, INSERT, UPDATE, DELETE ON event_checkins TO app_rw;
COMMIT;
