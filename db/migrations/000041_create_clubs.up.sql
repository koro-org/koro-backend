-- 000041_create_clubs.up.sql (02 §8.5). Club-specific data of an org of kind
-- 'club' (1:1). The sensitivity flag marks clubs whose membership is
-- special-category data (religion, politics, identity ...): the CHECKs keep
-- the category, flag, reason and stamp consistent. A club is closed through
-- its org, never deleted, so app_rw has no DELETE.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE clubs (
  org_id              uuid PRIMARY KEY,
  org_kind            text NOT NULL DEFAULT 'club' CONSTRAINT clubs_org_kind_chk CHECK (org_kind = 'club'),
  university_id       uuid NOT NULL CONSTRAINT clubs_university_id_fkey REFERENCES universities (id),
  category            text NOT NULL CONSTRAINT clubs_category_chk CHECK (category IN (
                        'academic','technology','career','entrepreneurship','arts','culture','sports',
                        'social_responsibility','hobby','religious','political','identity','other')),
  is_sensitive        boolean NOT NULL DEFAULT false,
  sensitive_reason    text CONSTRAINT clubs_sensitive_reason_chk CHECK (sensitive_reason IN ('religion','political_opinion','philosophical_belief',
                        'lgbtiq','ethnic_origin','health','union','other')),
  sensitivity_set_by  uuid CONSTRAINT clubs_sensitivity_set_by_fkey REFERENCES users (id),
  sensitivity_set_at  timestamptz,
  join_policy         text NOT NULL DEFAULT 'approval' CONSTRAINT clubs_join_policy_chk CHECK (join_policy IN ('open','approval')),
  claim_status        text NOT NULL DEFAULT 'unclaimed' CONSTRAINT clubs_claim_status_chk CHECK (claim_status IN ('unclaimed','claimed')),
  founded_year        smallint CONSTRAINT clubs_founded_year_chk CHECK (founded_year BETWEEN 1900 AND 2100),
  social_links        jsonb NOT NULL DEFAULT '{}',          -- {"instagram":"https://...", ...} https only
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT clubs_org_id_org_kind_fkey FOREIGN KEY (org_id, org_kind) REFERENCES organizations (id, kind),
  CONSTRAINT clubs_sensitive_category_chk CHECK (category NOT IN ('religious','political','identity') OR is_sensitive),
  CONSTRAINT clubs_sensitive_reason_set_chk CHECK (is_sensitive = (sensitive_reason IS NOT NULL)),
  CONSTRAINT clubs_sensitivity_stamp_chk CHECK (is_sensitive = (sensitivity_set_at IS NOT NULL))
);
CREATE INDEX clubs_university_idx ON clubs (university_id, category);
CREATE INDEX clubs_nonsensitive_idx ON clubs (university_id) WHERE NOT is_sensitive;
CREATE TRIGGER clubs_touch_updated_at BEFORE UPDATE ON clubs
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON clubs TO app_rw;
COMMIT;
