-- 000059_create_form_lint_rules.down.sql
-- DATA LOSS: every lint rule, the seeded ones and those added by the DPO.
BEGIN;
DROP TABLE IF EXISTS form_lint_rules;
COMMIT;
