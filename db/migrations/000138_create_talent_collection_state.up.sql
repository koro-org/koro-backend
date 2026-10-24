-- 000138_create_talent_collection_state.up.sql (BE-090; 00 T-321, user
-- decision of 2026-10-23). The reconciliation state of the talent ledger:
-- exactly one row, no personal data. While talent.default_enabled is false
-- the reversal consumers (registration.cancelled, checkin.voided,
-- post.unliked, membership.ended) write no ledger row (T-32) but count the
-- drop in reversals_dropped; the first talent.recompute with collection on
-- after a drop asks the source modules which credited sources were undone
-- meanwhile, writes their reversals and records the count it started from
-- in reconciled_drops (a drop during that run stays pending for the next
-- one). A counter, not a clock: workers' clocks may disagree. The row is
-- created by the first drop (upsert; no row = nothing dropped), so the
-- migrations write nothing here. app_talent only.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE talent.collection_state (
  id                 boolean PRIMARY KEY DEFAULT true CONSTRAINT collection_state_singleton_chk CHECK (id),
  reversals_dropped  bigint NOT NULL DEFAULT 0 CONSTRAINT collection_state_dropped_chk CHECK (reversals_dropped >= 0),
  last_dropped_at    timestamptz,  -- the latest drop (for operators)
  reconciled_drops   bigint NOT NULL DEFAULT 0
                     CONSTRAINT collection_state_reconciled_chk CHECK (reconciled_drops >= 0 AND reconciled_drops <= reversals_dropped),
  reconciled_at      timestamptz,  -- when the last reconciliation finished
  CONSTRAINT collection_state_last_dropped_chk CHECK (reversals_dropped = 0 OR last_dropped_at IS NOT NULL),
  CONSTRAINT collection_state_reconciled_at_chk CHECK (reconciled_drops = 0 OR reconciled_at IS NOT NULL)
);
GRANT SELECT, INSERT, UPDATE ON talent.collection_state TO app_talent;
COMMIT;
