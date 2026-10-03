-- 000017_add_fk_consents_user.down.sql
BEGIN;
ALTER TABLE consents DROP CONSTRAINT IF EXISTS consents_user_id_fkey;
COMMIT;
