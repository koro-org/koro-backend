-- 000075_create_segments.up.sql (02 §13.1). A saved audience of an org
-- (OR inside a dimension, AND across dimensions). The count is only ever
-- computed by count_segment (k >= 10): last_count is NULL or >= 10, never a
-- smaller number. Archived, not deleted (no DELETE for app_rw).
-- segments_id_org_uq (06 H-38, addition to 02): the target of the composite
-- FK of outreach_dispatches, so a dispatch cannot use another org's segment.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE segments (
  id                  uuid PRIMARY KEY DEFAULT uuidv7(),
  org_id              uuid NOT NULL CONSTRAINT segments_org_id_fkey REFERENCES organizations (id),
  name                text COLLATE "tr" NOT NULL CONSTRAINT segments_name_chk CHECK (char_length(name) BETWEEN 2 AND 100),
  definition          jsonb NOT NULL,     -- {"uni":[..],"dept":[..],"year":[3,4],"club":[..]}
  audience_keys       text[] NOT NULL,
  audience_dim_count  smallint NOT NULL CONSTRAINT segments_audience_dim_count_chk CHECK (audience_dim_count BETWEEN 1 AND 4),
  last_count          integer CONSTRAINT segments_last_count_chk CHECK (last_count IS NULL OR last_count >= 10),   -- k < 10 ⇒ NULL
  last_counted_at     timestamptz,
  created_by          uuid NOT NULL CONSTRAINT segments_created_by_fkey REFERENCES users (id),
  archived_at         timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT segments_id_org_uq UNIQUE (id, org_id)
);
CREATE INDEX segments_org_idx ON segments (org_id) WHERE archived_at IS NULL;
CREATE TRIGGER segments_touch_updated_at BEFORE UPDATE ON segments
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON segments TO app_rw;
COMMIT;
