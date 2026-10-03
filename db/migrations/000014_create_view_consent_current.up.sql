-- 000014_create_view_consent_current.up.sql (02 §4.3). The latest row of
-- every (user, purpose, scope): DISTINCT ON treats NULL scope_ids as equal,
-- so unscoped purposes collapse to one row per user and purpose. A current
-- row with action = 'withdrawn' means "no consent". Read-only for app_rw.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE VIEW consent_current AS
  SELECT DISTINCT ON (user_id, purpose_code, scope_id) *
  FROM consents
  ORDER BY user_id, purpose_code, scope_id, occurred_at DESC, id DESC;
GRANT SELECT ON consent_current TO app_rw;
COMMIT;
