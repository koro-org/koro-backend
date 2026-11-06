-- 000181_index_sms_codes_user_created.up.sql (BE-181). The daily ceiling of the
-- SMS password reset counts the codes of an account and a purpose in a rolling
-- day, consumed or not (identity SMSCodeUsage); sms_codes_open_idx holds the
-- open ones only. It also serves the erasure (DELETE by user_id).
-- Live table: one statement, CONCURRENTLY, no BEGIN (05 §7.2).
CREATE INDEX CONCURRENTLY IF NOT EXISTS sms_codes_user_created_idx
  ON sms_codes (user_id, purpose, created_at);
