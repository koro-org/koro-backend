-- 000186_index_sessions_user_history.up.sql (BE-209). The new sign-in alert
-- compares a sign-in with the account's newest session rows, ended and
-- revoked ones included (identity RecentSessions: user_id = ? ORDER BY id
-- DESC LIMIT n); sessions_user_active_idx holds the unrevoked ones only. It
-- also serves the KVKK export and erasure of a user's sessions.
-- Live table: one statement, CONCURRENTLY, no BEGIN (05 §7.2).
CREATE INDEX CONCURRENTLY IF NOT EXISTS sessions_user_history_idx
  ON sessions (user_id, id);
