-- loadtest/prep.sql (OPS-005): test-environment preparation after
-- `koro seed demo`, run once by the postgres superuser of the throwaway load
-- test database (loadtest/compose.yaml). Never run it anywhere else.
--
-- Only what an operator would otherwise publish or grant through the admin
-- surface (password reset + TOTP + four eyes) is written here; every user
-- action (the students' discovery opt-in, registrations, orders) goes
-- through the real API in the k6 setup. Idempotent; synthetic text only.
\set ON_ERROR_STOP on

-- Query statistics for the run's report (shared_preload_libraries in compose).
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- 1. A published consent text for discovery_profile: the demo seed publishes
--    only the privacy notice, and turning `discoverable` on needs the
--    current consent text (PATCH /api/v1/me/privacy, students module).
INSERT INTO notice_versions (kind, purpose_code, audience, locale, version, title, body_md, body_sha256, published_at, created_by)
SELECT 'consent_text', 'discovery_profile', 'student', 'tr-TR', '2026.10.24',
       'Keşif açık rıza metni (yük testi, sentetik)', t.body, sha256(convert_to(t.body, 'UTF8')),
       now() - interval '1 day', u.id
FROM (SELECT 'Yük testi ortamının sentetik açık rıza metnidir; gerçek bir kişiye ait değildir.'::text AS body) t
CROSS JOIN users u
WHERE u.email = 'demo.yonetici1@example.test'
  AND NOT EXISTS (SELECT 1 FROM notice_versions n WHERE n.kind = 'consent_text' AND n.purpose_code = 'discovery_profile'
                  AND n.audience = 'student' AND n.locale = 'tr-TR' AND n.published_at IS NOT NULL AND n.retired_at IS NULL);

-- 2. A discovery_access right for the main demo company (admin grant): the
--    demo seed grants targeted_post only; discovery search needs this.
INSERT INTO entitlements (org_id, feature, kind, valid_from, valid_to, status, source_type, granted_by, activated_at)
SELECT m.org_id, 'discovery_access', 'access', now() - interval '1 day', now() + interval '30 days', 'active',
       'admin_grant', a.id, now() - interval '1 day'
FROM users c
JOIN org_members m ON m.user_id = c.id AND m.role = 'company_owner'
CROSS JOIN users a
WHERE c.email = 'demo.sirket@example.test' AND a.email = 'demo.yonetici1@example.test'
  AND NOT EXISTS (SELECT 1 FROM entitlements e WHERE e.org_id = m.org_id AND e.feature = 'discovery_access'
                  AND e.status = 'active');
