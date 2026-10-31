-- 000169_create_conversations.up.sql (BE-162; user decision Q-40; 02 §14.3–14.5
-- doc patch). The in-platform conversation of a company and a student that
-- starts when the student accepts the company's contact request (until
-- BE-162 messaging stopped at the request: "konuşma Faz 2").
-- * conversations: one per accepted request (contact_request_id unique). The
--   request is swept 12 months after its acceptance (contact_request_closed)
--   while the conversation may go on: ON DELETE SET NULL. org_id is the
--   request's company (bound to org kind 'company' like contact_requests;
--   06 H-38: the store copies it from the request row), company_user_id the
--   member who sent the request (the company's recipient of the notices).
--   company_unanswered counts the company's messages since the student's
--   last one (the service caps it). last_activity_at is the last message or
--   the closing moment: the list order and the retention anchor.
--   06 H-37: one complete CHECK per status (no "(status) = (A AND B)").
-- * conversation_messages: immutable (no UPDATE for app_rw); go with their
--   conversation (CASCADE).
-- * conversation_user_states: each participant's read marker and mute.
-- * Personal data: the messaging KVKK provider exports and erases them; the
--   retention conversation_closed deletes a conversation (messages and states
--   with it) 12 months after it closed or, while open, after its last
--   message (seeded unapproved, T-35).
-- * Backfill: an open conversation for every accepted request whose student
--   has not blocked the company (staging data of BE-056/BE-089).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE conversations (
  id                  uuid PRIMARY KEY DEFAULT uuidv7(),
  contact_request_id  uuid CONSTRAINT conversations_contact_request_id_fkey REFERENCES contact_requests (id) ON DELETE SET NULL,
  org_id              uuid NOT NULL,
  org_kind            text NOT NULL DEFAULT 'company' CONSTRAINT conversations_org_kind_chk CHECK (org_kind = 'company'),
  student_user_id     uuid NOT NULL CONSTRAINT conversations_student_user_id_fkey REFERENCES users (id),
  company_user_id     uuid NOT NULL CONSTRAINT conversations_company_user_id_fkey REFERENCES users (id),
  status              text NOT NULL DEFAULT 'open' CONSTRAINT conversations_status_chk CHECK (status IN ('open','closed')),
  closed_by           text CONSTRAINT conversations_closed_by_chk CHECK (closed_by IN ('student','company')),
  closed_at           timestamptz,
  company_unanswered  integer NOT NULL DEFAULT 0 CONSTRAINT conversations_company_unanswered_chk CHECK (company_unanswered >= 0),
  last_activity_at    timestamptz NOT NULL,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT conversations_org_id_org_kind_fkey FOREIGN KEY (org_id, org_kind) REFERENCES organizations (id, kind),
  CONSTRAINT conversations_parties_chk CHECK (student_user_id <> company_user_id),
  CONSTRAINT conversations_open_chk CHECK (status <> 'open' OR (closed_by IS NULL AND closed_at IS NULL)),
  CONSTRAINT conversations_closed_chk CHECK (status <> 'closed' OR (closed_by IS NOT NULL AND closed_at IS NOT NULL))
);
CREATE UNIQUE INDEX conversations_contact_request_uq ON conversations (contact_request_id);
CREATE INDEX conversations_org_idx      ON conversations (org_id, last_activity_at DESC, id DESC);
CREATE INDEX conversations_student_idx  ON conversations (student_user_id, last_activity_at DESC, id DESC);
CREATE INDEX conversations_activity_idx ON conversations (last_activity_at);
CREATE TRIGGER conversations_touch_updated_at BEFORE UPDATE ON conversations
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON conversations TO app_rw;

CREATE TABLE conversation_messages (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  conversation_id  uuid NOT NULL CONSTRAINT conversation_messages_conversation_id_fkey REFERENCES conversations (id) ON DELETE CASCADE,
  sender_side      text NOT NULL CONSTRAINT conversation_messages_sender_side_chk CHECK (sender_side IN ('student','company')),
  sender_user_id   uuid NOT NULL CONSTRAINT conversation_messages_sender_user_id_fkey REFERENCES users (id),
  body             text NOT NULL CONSTRAINT conversation_messages_body_chk CHECK (char_length(body) BETWEEN 1 AND 2000),
  created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX conversation_messages_conversation_idx ON conversation_messages (conversation_id, id DESC);
CREATE INDEX conversation_messages_sender_idx       ON conversation_messages (sender_user_id);
GRANT SELECT, INSERT, DELETE ON conversation_messages TO app_rw;

CREATE TABLE conversation_user_states (
  conversation_id       uuid NOT NULL CONSTRAINT conversation_user_states_conversation_id_fkey REFERENCES conversations (id) ON DELETE CASCADE,
  user_id               uuid NOT NULL CONSTRAINT conversation_user_states_user_id_fkey REFERENCES users (id),
  last_read_message_id  uuid,
  read_at               timestamptz,
  muted_at              timestamptz,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (conversation_id, user_id),
  CONSTRAINT conversation_user_states_read_chk CHECK ((last_read_message_id IS NULL) = (read_at IS NULL))
);
CREATE INDEX conversation_user_states_user_idx ON conversation_user_states (user_id);
CREATE TRIGGER conversation_user_states_touch_updated_at BEFORE UPDATE ON conversation_user_states
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE, DELETE ON conversation_user_states TO app_rw;

INSERT INTO conversations (contact_request_id, org_id, student_user_id, company_user_id, last_activity_at, created_at)
SELECT r.id, r.org_id, r.student_user_id, r.sender_user_id, r.responded_at, r.responded_at
FROM contact_requests r
WHERE r.status = 'accepted'
  AND NOT EXISTS (SELECT 1 FROM contact_blocks b WHERE b.student_user_id = r.student_user_id AND b.company_org_id = r.org_id);

-- The retention of the conversations (02 §24.2 doc patch; messaging's sweeper).
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('conversation_closed', 'messaging', 'conversations', '12 months', 'closed_at', 'hard_delete', 'Uyuşmazlık; veri minimizasyonu', true,
   'Şirket–öğrenci konuşmaları mesajları ve okunma/sessiz durumlarıyla kapanıştan, açık kaldıysa son mesajdan 12 ay sonra silinir.')
ON CONFLICT (code) DO NOTHING;
COMMIT;
