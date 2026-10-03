-- 000121_create_retention_purge_and_analytics_views.down.sql
-- Drops the analytics views and the purge functions, restores the append-only
-- guards of 000002/000074 byte for byte, and puts the role_history anchor and
-- the dsr_export_file period back (unapproved rows only). A policy that uses
-- the anchor 'revoked_or_rejected' after an approval blocks the down: the
-- restored CHECK fails (intended; decide by hand).
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP VIEW IF EXISTS analytics.talent_distribution;
DROP VIEW IF EXISTS analytics.event_participation_monthly;
DROP VIEW IF EXISTS analytics.students_by_university;
DROP VIEW IF EXISTS analytics.signups_monthly;

DELETE FROM public.retention_policies WHERE code = 'privileged_action_history'
  AND NOT EXISTS (SELECT 1 FROM public.destruction_logs WHERE policy_code = 'privileged_action_history');
UPDATE public.retention_policies SET retention = interval '30 days'
 WHERE code = 'dsr_export_file' AND retention = interval '7 days' AND NOT legal_approved;
UPDATE public.retention_policies SET anchor = 'ended_at'
 WHERE code = 'role_history' AND anchor = 'revoked_or_rejected' AND NOT legal_approved;
ALTER TABLE public.retention_policies DROP CONSTRAINT retention_policies_anchor_chk;
ALTER TABLE public.retention_policies ADD CONSTRAINT retention_policies_anchor_chk CHECK (anchor IN ('created_at','ended_at','closed_at',
  'expires_at','event_end','decided_at','withdrawn_or_erased','last_seen_at','published_at'));

DROP FUNCTION IF EXISTS public.privileged_account_actions_purge(timestamptz, integer, boolean);
DROP FUNCTION IF EXISTS talent.config_versions_purge(timestamptz, integer, boolean);
DROP FUNCTION IF EXISTS talent.point_events_erase_user(uuid);
DROP FUNCTION IF EXISTS talent.point_events_purge(timestamptz, integer, boolean);
DROP FUNCTION IF EXISTS public.destruction_logs_purge(timestamptz, integer, boolean);
DROP FUNCTION IF EXISTS public.moderation_actions_purge(timestamptz, integer, boolean);
DROP FUNCTION IF EXISTS public.consents_purge(timestamptz, integer, boolean);
DROP FUNCTION IF EXISTS public.consents_purgeable(timestamptz);
DROP FUNCTION IF EXISTS public.koro_retention_cutoff(text, timestamptz, integer, boolean);

CREATE OR REPLACE FUNCTION public.moderation_actions_forbid_mutation() RETURNS trigger
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

CREATE OR REPLACE FUNCTION public.koro_forbid_mutation() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% on %.% is not allowed: the table is append-only', TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
    USING ERRCODE = 'restrict_violation';
END $$;
COMMIT;
