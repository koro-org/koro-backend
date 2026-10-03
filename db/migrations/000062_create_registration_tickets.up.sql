-- 000062_create_registration_tickets.up.sql (02 §11.7). The QR ticket of a
-- registration. Only the SHA-256 of the token is stored (the token itself is
-- derived from the registration id and version, R-003 §2.5.4); a re-issue is
-- version + 1 and revokes the old one. At most one active ticket per
-- registration. Deleted with the registration (ON DELETE CASCADE).
-- 02 fix (H-20): the revoked-state CHECK is split in two. The original
-- `(status = 'revoked') = (revoked_at IS NOT NULL AND revoke_reason IS NOT NULL)`
-- also accepted an active ticket carrying a revoked_at without a reason (and
-- one with a reason but no revoked_at).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE registration_tickets (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  registration_id  uuid NOT NULL CONSTRAINT registration_tickets_registration_id_fkey REFERENCES event_registrations (id) ON DELETE CASCADE,
  event_id         uuid NOT NULL CONSTRAINT registration_tickets_event_id_fkey REFERENCES events (id),
  version          integer NOT NULL CONSTRAINT registration_tickets_version_chk CHECK (version > 0),
  key_id           text NOT NULL,
  token_lookup     bytea NOT NULL CONSTRAINT registration_tickets_token_lookup_uq UNIQUE
                     CONSTRAINT registration_tickets_token_lookup_chk CHECK (octet_length(token_lookup) = 32),   -- SHA-256(token bytes)
  manual_code      text NOT NULL CONSTRAINT registration_tickets_manual_code_chk CHECK (manual_code ~ '^[0-9A-HJKMNP-TV-Z]{8}$'),   -- Crockford base32, 40 bit
  status           text NOT NULL DEFAULT 'active' CONSTRAINT registration_tickets_status_chk CHECK (status IN ('active','revoked')),
  issued_at        timestamptz NOT NULL DEFAULT now(),
  revoked_at       timestamptz,
  revoke_reason    text CONSTRAINT registration_tickets_revoke_reason_chk CHECK (revoke_reason IN ('reissued','registration_cancelled','event_cancelled','admin')),
  CONSTRAINT registration_tickets_registration_version_uq UNIQUE (registration_id, version),
  CONSTRAINT registration_tickets_event_manual_code_uq UNIQUE (event_id, manual_code),
  CONSTRAINT registration_tickets_revoked_chk CHECK ((status = 'revoked') = (revoked_at IS NOT NULL) AND (status = 'revoked') = (revoke_reason IS NOT NULL))
);
CREATE UNIQUE INDEX registration_tickets_active_uq ON registration_tickets (registration_id) WHERE status = 'active';
GRANT SELECT, INSERT, UPDATE, DELETE ON registration_tickets TO app_rw;
COMMIT;
