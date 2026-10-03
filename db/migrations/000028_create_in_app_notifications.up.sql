-- 000028_create_in_app_notifications.up.sql (02 §6.4). In-app notification
-- inbox. source_org_id gets its FK in 000032 (after organizations).
-- link_path must be an in-app relative path: a leading '//' would be a
-- protocol-relative URL (open redirect), so it is rejected (02 §6.4 fix).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE in_app_notifications (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id        uuid NOT NULL CONSTRAINT in_app_notifications_user_id_fkey REFERENCES users (id),
  kind           text NOT NULL CONSTRAINT in_app_notifications_kind_chk CHECK (kind ~ '^[a-z_]+(\.[a-z_]+)*$'),
  title          text NOT NULL CONSTRAINT in_app_notifications_title_chk CHECK (char_length(title) <= 140),
  body           text CONSTRAINT in_app_notifications_body_chk CHECK (char_length(body) <= 500),
  link_path      text CONSTRAINT in_app_notifications_link_path_chk CHECK (link_path ~ '^/[A-Za-z0-9/_\-?=&.]*$' AND link_path NOT LIKE '//%'),   -- in-app relative path only
  source_org_id  uuid,                                              -- FK organizations (000032)
  data           jsonb NOT NULL DEFAULT '{}',
  read_at        timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now()                 -- read_at is the only column that changes
);
CREATE INDEX in_app_notifications_user_idx   ON in_app_notifications (user_id, created_at DESC, id DESC);
CREATE INDEX in_app_notifications_unread_idx ON in_app_notifications (user_id) WHERE read_at IS NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON in_app_notifications TO app_rw;
COMMIT;
