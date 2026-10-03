-- 000035_create_org_invitations.down.sql
-- DATA LOSS: open invitations (invitees must be invited again).
BEGIN;
DROP TABLE IF EXISTS org_invitations;
COMMIT;
