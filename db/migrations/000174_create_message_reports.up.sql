-- 000174_create_message_reports.up.sql (BE-162b; user decision Q-43, the
-- lawyer's opinion of 2026-10-31, T-405). A party of a conversation reports a
-- message of the other side; the report joins the platform's moderation
-- queue (content_reports, target_type 'message') and keeps what was reported:
-- the reported message and the messages before it in its conversation, as
-- they stood when the report was filed (the conversation itself goes 12
-- months after its last activity, conversation_closed, and a closed one is
-- never edited, but the evidence must outlive it).
--   * content_reports_target_type_chk takes 'message' (target_id = the
--     reported message). content_reports_id_target_uq (id, target_type,
--     target_id) lets message_reports point at a message report of its own
--     message only (composite FK; id is already the key, the constraint costs
--     one index).
--   * message_reports: one per message report: the conversation, its company,
--     its student and the side of the reported message, copied from the
--     conversation (06 H-38) when the report is filed (taken_at). No FK to
--     the conversation or the message: they may go first, the copy stays.
--   * message_report_messages: the reported message and at most the 5 before
--     it (the service's cap, Q-43 "yalnız şikâyet edilen mesaj + öncesindeki
--     birkaç mesaj"), each as written (body 1–2000 as conversation_messages).
--   * Written once with the report: app_rw has no UPDATE on either table.
--     They go with their report (ON DELETE CASCADE): the reporter's erasure
--     and the content_report retention (12 months after the resolution). No
--     new policy; the seeded content_report text is left as 000168 wrote it
--     (its down migration restores that exact text): 02 §24.2 names the copy.
--   * Read by the platform's moderators only, one report at a time, each
--     read with a justification and an audit record (Q-43).
BEGIN;
SET LOCAL lock_timeout = '5s';

ALTER TABLE content_reports
  DROP CONSTRAINT content_reports_target_type_chk,
  ADD CONSTRAINT content_reports_target_type_chk CHECK (target_type IN ('post','org','message')),
  ADD CONSTRAINT content_reports_id_target_uq UNIQUE (id, target_type, target_id);

CREATE TABLE message_reports (
  report_id        uuid PRIMARY KEY,
  report_type      text NOT NULL DEFAULT 'message' CONSTRAINT message_reports_report_type_chk CHECK (report_type = 'message'),
  message_id       uuid NOT NULL,                     -- the reported message (no FK: the conversation may go first)
  conversation_id  uuid NOT NULL,                     -- no FK, as message_id
  org_id           uuid NOT NULL CONSTRAINT message_reports_org_id_fkey REFERENCES organizations (id),
  student_user_id  uuid NOT NULL CONSTRAINT message_reports_student_user_id_fkey REFERENCES users (id),
  reported_side    text NOT NULL CONSTRAINT message_reports_reported_side_chk CHECK (reported_side IN ('student','company')),
  taken_at         timestamptz NOT NULL,
  CONSTRAINT message_reports_report_fkey FOREIGN KEY (report_id, report_type, message_id)
    REFERENCES content_reports (id, target_type, target_id) ON DELETE CASCADE
);
CREATE INDEX message_reports_student_idx ON message_reports (student_user_id);
GRANT SELECT, INSERT, DELETE ON message_reports TO app_rw;

CREATE TABLE message_report_messages (
  report_id       uuid NOT NULL CONSTRAINT message_report_messages_report_id_fkey REFERENCES message_reports (report_id) ON DELETE CASCADE,
  message_id      uuid NOT NULL,
  sender_side     text NOT NULL CONSTRAINT message_report_messages_sender_side_chk CHECK (sender_side IN ('student','company')),
  sender_user_id  uuid NOT NULL CONSTRAINT message_report_messages_sender_user_id_fkey REFERENCES users (id),
  body            text NOT NULL CONSTRAINT message_report_messages_body_chk CHECK (char_length(body) BETWEEN 1 AND 2000),
  sent_at         timestamptz NOT NULL,
  PRIMARY KEY (report_id, message_id)
);
CREATE INDEX message_report_messages_sender_idx ON message_report_messages (sender_user_id);
GRANT SELECT, INSERT, DELETE ON message_report_messages TO app_rw;
COMMIT;
