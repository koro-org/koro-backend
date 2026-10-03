-- 000017_add_fk_consents_user.up.sql (02 §5.1). The forward FK of consents
-- (000013) now that users exists.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE consents ADD CONSTRAINT consents_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id);
COMMIT;
