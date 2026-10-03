-- 000059_create_form_lint_rules.up.sql (02 §11.5). The KVKK lint rules of the
-- event form, kept as data so that the DPO can tune them without a release
-- (events.lint_rule.manage). Rules are deactivated (active = false), never
-- deleted, so app_rw has no DELETE. pattern is an RE2 expression run on the
-- koro_norm-ed label / help text; an empty pattern would match every form,
-- hence the lower bound of 1 (an addition to the 02 DDL).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE form_lint_rules (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  class           text NOT NULL CONSTRAINT form_lint_rules_class_chk CHECK (class IN ('blocked','warn_special','review_identifier')),
  pattern         text NOT NULL CONSTRAINT form_lint_rules_pattern_chk CHECK (char_length(pattern) BETWEEN 1 AND 500),
  description_tr  text NOT NULL,
  active          boolean NOT NULL DEFAULT true,
  updated_by      uuid CONSTRAINT form_lint_rules_updated_by_fkey REFERENCES users (id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER form_lint_rules_touch_updated_at BEFORE UPDATE ON form_lint_rules
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON form_lint_rules TO app_rw;
COMMIT;
