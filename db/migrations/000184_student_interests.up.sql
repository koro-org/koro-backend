-- 000184_student_interests.up.sql (BE-204; user decision Q-57, uyum denetimi E-2).
-- The interests a student declares on the profile: a signal of the student's
-- own rule-based recommendations (GET /me/recommendations, reason
-- matches_interest), shown to the student alone, never to a company, never
-- an audience key or a talent signal.
-- * The values are a fixed list: the club categories that are not special
--   categories of personal data (clubs_category_chk without religious,
--   political, identity and other; KVKK md. 6: no religion, political
--   opinion, health, sexual life or ethnic origin is ever an interest). A new
--   value is a migration (students.Interests, recommend's mapping).
-- * At most 5; the service keeps them without duplicates in the list's order.
-- * KVKK: the student's own data on an existing personal table (students'
--   provider): exported with the profile and deleted with it (kept while the
--   account is).
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE student_profiles
  ADD COLUMN interests text[] NOT NULL DEFAULT '{}'
    CONSTRAINT student_profiles_interests_chk CHECK (
      cardinality(interests) <= 5
      AND interests <@ ARRAY['academic','technology','career','entrepreneurship','arts','culture','sports',
                             'social_responsibility','hobby']::text[]
    );
COMMIT;
