-- 000132_index_audit_events_break_glass_subject.up.sql (BE-078). listMySecurityEvents
-- reads the break-glass accesses to a user's data by subject, newest first; without
-- this index its query scanned the whole audit log. Live table: one statement,
-- CONCURRENTLY, no BEGIN (05 §7.2).
CREATE INDEX CONCURRENTLY IF NOT EXISTS audit_events_break_glass_subject_idx
  ON audit_events ((metadata->>'subject_user_id'), occurred_at DESC)
  WHERE metadata @> '{"break_glass": true}';
