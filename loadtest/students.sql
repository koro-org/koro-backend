-- loadtest/students.sql (OPS-005): the synthetic demo students k6 signs in
-- as (all @example.test, from `koro seed demo`), printed as one JSON array.
-- Adults with an active verification only (audience_eligible), without the
-- club admin (a student too, but the organiser in the scenarios).
SELECT coalesce(json_agg(u.email ORDER BY u.email), '[]'::json)
FROM users u
JOIN student_profiles sp ON sp.user_id = u.id AND sp.audience_eligible
WHERE u.signup_persona = 'student' AND u.status = 'active' AND u.password_hash IS NOT NULL
  AND u.email LIKE '%@example.test' AND u.email <> 'demo.kulup@example.test';
