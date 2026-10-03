-- 000104_add_fk_orders_collection_account.up.sql (02 §20.1, §23). The
-- forward FK of orders (000101) to the collection account the buyer was told
-- to pay into; orders is empty when this runs, so the FK is validated at
-- once.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE orders ADD CONSTRAINT orders_collection_account_id_fkey
  FOREIGN KEY (collection_account_id) REFERENCES collection_accounts (id);
COMMIT;
