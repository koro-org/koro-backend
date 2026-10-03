-- 000031_seed_platform_org.up.sql (02 §23). The single platform org: the
-- "author" of platform content and the org of platform staff memberships
-- (kind 'platform', role 'staff'). Code finds it by kind; the id is the
-- database default. ON CONFLICT DO NOTHING keeps a re-run harmless (the
-- partial unique index organizations_single_platform_uq allows one).
BEGIN;
SET LOCAL lock_timeout = '5s';
INSERT INTO organizations (kind, name, slug, status, description)
VALUES ('platform', 'Koro', 'koro', 'active', 'Koro platformu.')
ON CONFLICT DO NOTHING;
COMMIT;
