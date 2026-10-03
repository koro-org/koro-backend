-- 000074_create_moderation_actions.up.sql (02 §12.10). Append-only log of
-- moderation decisions. Two layers (02 §0.5): app_rw has SELECT/INSERT only,
-- and the moderation_actions_immutable trigger blocks UPDATE/DELETE for
-- everyone, the owner included, with one exception: report_id may go from a
-- report to NULL with every other column unchanged. That is what the
-- ON DELETE SET NULL of report_id does when the report is hard-deleted after
-- its retention period (content_report: 12 months); the action itself is
-- kept for 2 years (moderation_action). TRUNCATE is blocked as well.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE moderation_actions (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  report_id      uuid CONSTRAINT moderation_actions_report_id_fkey REFERENCES content_reports (id) ON DELETE SET NULL,
  target_type    text NOT NULL CONSTRAINT moderation_actions_target_type_chk CHECK (target_type IN ('post','org')),
  target_id      uuid NOT NULL,                                     -- no FK: polymorphic
  action         text NOT NULL CONSTRAINT moderation_actions_action_chk CHECK (action IN ('approve','hide','remove','restore','warn_org','restrict_org','suspend_org')),
  reason_code    text NOT NULL CONSTRAINT moderation_actions_reason_code_chk CHECK (reason_code ~ '^[a-z_]{2,40}$'),
  actor_user_id  uuid NOT NULL CONSTRAINT moderation_actions_actor_user_id_fkey REFERENCES users (id),
  note           text CONSTRAINT moderation_actions_note_chk CHECK (char_length(note) <= 1000),
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX moderation_actions_target_idx ON moderation_actions (target_type, target_id, created_at);

-- koro_forbid_mutation with the report_id SET NULL exception (02 §12.10).
CREATE FUNCTION public.moderation_actions_forbid_mutation() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE'
     AND OLD.report_id IS NOT NULL AND NEW.report_id IS NULL
     AND (to_jsonb(NEW) - 'report_id') = (to_jsonb(OLD) - 'report_id') THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION '% on %.% is not allowed: the table is append-only (only report_id may be cleared)', TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
    USING ERRCODE = 'restrict_violation';
END $$;
CREATE TRIGGER moderation_actions_immutable BEFORE UPDATE OR DELETE ON moderation_actions
  FOR EACH ROW EXECUTE FUNCTION public.moderation_actions_forbid_mutation();
CREATE TRIGGER moderation_actions_no_truncate BEFORE TRUNCATE ON moderation_actions
  FOR EACH STATEMENT EXECUTE FUNCTION public.koro_forbid_mutation();
GRANT SELECT, INSERT ON moderation_actions TO app_rw;
COMMIT;
