-- 000078_create_contact_blocks.up.sql (02 §14.2). Schema only (phase 3): a
-- student blocked contact requests of a company. Belongs to the student; the
-- company never learns that it exists.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE contact_blocks (
  student_user_id  uuid NOT NULL CONSTRAINT contact_blocks_student_user_id_fkey REFERENCES users (id),
  company_org_id   uuid NOT NULL CONSTRAINT contact_blocks_company_org_id_fkey REFERENCES organizations (id),
  created_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT contact_blocks_pkey PRIMARY KEY (student_user_id, company_org_id)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON contact_blocks TO app_rw;
COMMIT;
