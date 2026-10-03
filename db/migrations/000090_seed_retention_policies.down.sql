-- 000090_seed_retention_policies.down.sql
-- DATA LOSS: the seeded retention policies with their approvals (policies
-- added later stay).
BEGIN;
DELETE FROM retention_policies WHERE code IN (
  'outbox_published', 'webhook_payload', 'webhook_row', 'idempotency', 'audit_events', 'audit_roots', 'files_orphan',
  'edu_document', 'event_export_file', 'dsr_export_file', 'consent_proof', 'session_expired', 'email_token',
  'edu_verification_history', 'role_history', 'notification_delivery', 'email_suppression', 'push_token',
  'in_app_notification', 'membership_ended', 'membership_ended_sensitive', 'org_invitation', 'club_claim',
  'access_grant_revoked', 'event_form_answers', 'event_form_answers_sensitive', 'event_staff', 'event_export_log',
  'post_removed', 'content_report', 'moderation_action', 'outreach_dispatch', 'contact_request_closed',
  'talent_ledger', 'talent_counters', 'talent_runs', 'talent_neutralization', 'entitlement_other',
  'financial_records', 'dsr_record', 'destruction_log', 'breach_record', 'dsr_erasure', 'dormant_account');
COMMIT;
