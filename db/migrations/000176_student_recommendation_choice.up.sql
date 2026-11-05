-- 000176_student_recommendation_choice.up.sql (BE-174; uyum denetimi #25).
-- The student's choice about the rule-based recommendations of
-- GET /me/recommendations: 'default' follows the product setting (on for an
-- adult, off for a minor: recommend.Enabled), 'enabled' and 'disabled' are
-- the student's own answer. Like activity_ranking_choice, the row keeps no
-- product default of its own, so a later change of the default reaches every
-- student who never chose. The recommendations read the student's own data
-- for the student only (no profiling for a third party, KVKK md. 5/2-c, f);
-- a student who turns them off gets none (KVKK md. 11 objection, in-product).
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE student_privacy_settings
  ADD COLUMN recommendation_choice text NOT NULL DEFAULT 'default'
    CONSTRAINT student_privacy_settings_recommendation_choice_chk CHECK (recommendation_choice IN ('default','enabled','disabled'));
COMMIT;
