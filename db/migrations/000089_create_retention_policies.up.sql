-- 000089_create_retention_policies.up.sql (02 §16.2). Retention periods are
-- data, not code: the sweepers read them (01 §7). Every change is audited
-- (retention_policy.update). Retired by enabled = false, never deleted (no
-- DELETE for app_rw; destruction_logs refer to the codes).
-- Additions to 02 (H-20):
--   * retention_policies_retention_chk: a negative period would put the
--     cut-off in the future and destroy live data;
--   * retention_policies_approval_chk: a legal approval names its reference
--     (document / meeting), so legal_approved cannot be flipped without it.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE retention_policies (
  code             text PRIMARY KEY CONSTRAINT retention_policies_code_chk CHECK (code ~ '^[a-z_]{3,60}$'),
  owner_module     text NOT NULL CONSTRAINT retention_policies_owner_module_chk CHECK (owner_module ~ '^[a-z_]+$'),
  target           text NOT NULL,                        -- table name or "files:<purpose>"
  retention        interval NOT NULL CONSTRAINT retention_policies_retention_chk CHECK (retention >= interval '0'),
  anchor           text NOT NULL CONSTRAINT retention_policies_anchor_chk CHECK (anchor IN ('created_at','ended_at','closed_at','expires_at','event_end',
                     'decided_at','withdrawn_or_erased','last_seen_at','published_at')),
  method           text NOT NULL CONSTRAINT retention_policies_method_chk CHECK (method IN ('hard_delete','anonymize','null_columns','file_delete')),
  legal_basis      text NOT NULL,
  legal_approved   boolean NOT NULL DEFAULT false,       -- approved by the lawyer / the accountant
  approved_ref     text,                                 -- reference of the approval document / meeting
  enabled          boolean NOT NULL DEFAULT true,
  description_tr   text NOT NULL,
  updated_by       uuid CONSTRAINT retention_policies_updated_by_fkey REFERENCES users (id),
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT retention_policies_approval_chk CHECK (NOT legal_approved OR approved_ref IS NOT NULL)
);
CREATE TRIGGER retention_policies_touch_updated_at BEFORE UPDATE ON retention_policies
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON retention_policies TO app_rw;
COMMIT;
