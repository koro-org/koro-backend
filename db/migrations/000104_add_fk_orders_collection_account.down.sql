-- 000104_add_fk_orders_collection_account.down.sql
BEGIN;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_collection_account_id_fkey;
COMMIT;
