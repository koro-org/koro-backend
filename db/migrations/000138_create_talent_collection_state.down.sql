-- 000138_create_talent_collection_state.down.sql
-- DATA LOSS: the reconciliation state (a pending reconciliation of the
-- reversals dropped while collection was off is forgotten).
BEGIN;
DROP TABLE IF EXISTS talent.collection_state;
COMMIT;
