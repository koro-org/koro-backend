-- loadtest/bulk.sql (BE-193): list volume for the filtered public lists of
-- the load test (BA §6.3: "10.000 kayıtla yanıt süresi < 1 sn"). Run once by
-- the postgres superuser of the throwaway load test database
-- (loadtest/compose.yaml) after `koro seed demo` and loadtest/prep.sql.
-- Never run it anywhere else.
--
--   psql -v events=10000 -v clubs=1000 -f loadtest/bulk.sql
--
-- Generated in the database (generate_series), nothing large in the repo.
-- Every row is synthetic: numbered club and event names built from a topic
-- and a kind ("Robotik Atölyesi 123"), no person, no address, online links
-- on example.test. The list endpoints read these tables only, so the rows
-- go in directly; the write paths (registrations, tickets, orders) are
-- measured through the API by k6, on the demo seed's own events.
--
--   - `clubs` synthetic clubs (default 1000), active and claimed, spread
--     over the active universities, non-sensitive categories only;
--   - `events` events (default 10000) of those clubs, the demo clubs
--     (sensitive ones excepted) and the universities: 80 % upcoming
--     (published; every 50th cancelled), 20 % ended (completed); 90 %
--     public, 5 % university-only, 5 % members-only; every category,
--     three formats and ten cities, start times over the next 240 days and
--     the last 180.
--
-- Idempotent (fixed slugs, ON CONFLICT DO NOTHING); ends with ANALYZE so
-- the planner sees the volume as it would in production.
\set ON_ERROR_STOP on
\if :{?events}
\else
  \set events 10000
\endif
\if :{?clubs}
\else
  \set clubs 1000
\endif

BEGIN;

-- Topic → club category (non-sensitive: clubs_sensitive_category_chk).
CREATE TEMP TABLE lt_topics (i int PRIMARY KEY, topic text NOT NULL, club_category text NOT NULL) ON COMMIT DROP;
INSERT INTO lt_topics VALUES
  (1, 'Kariyer', 'career'), (2, 'Yapay Zekâ', 'technology'), (3, 'Girişimcilik', 'entrepreneurship'),
  (4, 'Robotik', 'technology'), (5, 'Fotoğrafçılık', 'arts'), (6, 'Tiyatro', 'arts'), (7, 'Satranç', 'hobby'),
  (8, 'Veri Bilimi', 'academic'), (9, 'Siber Güvenlik', 'technology'), (10, 'Doğa Sporları', 'sports'),
  (11, 'Müzik', 'arts'), (12, 'Sinema', 'culture'), (13, 'Edebiyat', 'culture'),
  (14, 'Gönüllülük', 'social_responsibility'), (15, 'Havacılık', 'academic'), (16, 'Ekonomi', 'academic'),
  (17, 'Tasarım', 'arts'), (18, 'Oyun Geliştirme', 'technology'), (19, 'Dans', 'sports'), (20, 'Münazara', 'other');

-- Event kind → event category (every value of events_category_chk).
CREATE TEMP TABLE lt_kinds (i int PRIMARY KEY, kind text NOT NULL, category text NOT NULL) ON COMMIT DROP;
INSERT INTO lt_kinds VALUES
  (1, 'Atölyesi', 'workshop'), (2, 'Söyleşisi', 'talk'), (3, 'Yarışması', 'competition'), (4, 'Kariyer Günü', 'career'),
  (5, 'Buluşması', 'social'), (6, 'Gezisi', 'trip'), (7, 'Turnuvası', 'sports'), (8, 'Festivali', 'culture'),
  (9, 'Gönüllü Çalışması', 'volunteering'), (10, 'Semineri', 'other');

CREATE TEMP TABLE lt_cities (i int PRIMARY KEY, city text NOT NULL) ON COMMIT DROP;
INSERT INTO lt_cities VALUES
  (1, 'Ankara'), (2, 'İstanbul'), (3, 'İzmir'), (4, 'Bursa'), (5, 'Eskişehir'),
  (6, 'Antalya'), (7, 'Konya'), (8, 'Trabzon'), (9, 'Kayseri'), (10, 'Gaziantep');

CREATE TEMP TABLE lt_unis ON COMMIT DROP AS
SELECT row_number() OVER (ORDER BY u.yok_code) AS rn, u.id, u.org_id, u.city
FROM universities u JOIN organizations o ON o.id = u.org_id
WHERE u.is_active AND o.status = 'active';

SELECT count(*) AS lt_uni_count, (count(*) > 0) AS lt_have_unis FROM lt_unis \gset
\if :lt_have_unis
\else
  DO $$ BEGIN RAISE EXCEPTION 'bulk.sql: no active university (run koro seed reference and koro seed demo first)'; END $$;
\endif

-- The demo club admin creates the events (events.created_by is NOT NULL).
SELECT id AS lt_creator FROM users WHERE email = 'demo.kulup@example.test' \gset

-- 1. Synthetic clubs.
INSERT INTO organizations (kind, name, slug, status, city, description, verified_at)
SELECT 'club', format('%s Kulübü %s', t.topic, g), format('yuk-testi-kulubu-%s', g), 'active', u.city,
       'Yük testi için üretilmiş sentetik kulüp; gerçek bir topluluğa ait değildir.', now()
FROM generate_series(1, :clubs) g
JOIN lt_topics t ON t.i = 1 + g % 20
JOIN lt_unis u ON u.rn = 1 + g % :lt_uni_count
ON CONFLICT (slug) DO NOTHING;

INSERT INTO clubs (org_id, university_id, category, join_policy, claim_status)
SELECT o.id, u.id, t.club_category, 'open', 'claimed'
FROM generate_series(1, :clubs) g
JOIN organizations o ON o.slug = format('yuk-testi-kulubu-%s', g)
JOIN lt_topics t ON t.i = 1 + g % 20
JOIN lt_unis u ON u.rn = 1 + g % :lt_uni_count
ON CONFLICT (org_id) DO NOTHING;

-- 2. Organisers: every active non-sensitive club and every university org.
CREATE TEMP TABLE lt_orgs ON COMMIT DROP AS
SELECT row_number() OVER (ORDER BY x.org_id) AS rn, x.org_id, x.uni_id
FROM (
  SELECT c.org_id, c.university_id AS uni_id
  FROM clubs c JOIN organizations o ON o.id = c.org_id
  WHERE o.status = 'active' AND NOT c.is_sensitive
  UNION ALL
  SELECT u.org_id, u.id FROM lt_unis u
) x;
SELECT count(*) AS lt_org_count FROM lt_orgs \gset

-- 3. Events. g spreads the values independently: topic g % 20, kind
-- (g / 20) % 10, city (g * 31) % 10, organiser (g * 7919) % orgs.
INSERT INTO events (org_id, created_by, title, slug, summary, description_text, category, format, location_name, city,
                    online_url, starts_at, ends_at, visibility, visibility_university_id, status, published_at, cancelled_at,
                    cancel_reason, requires_registration, registration_mode, capacity)
SELECT o.org_id, :'lt_creator',
       format('%s %s %s', t.topic, k.kind, s.g),
       format('yuk-testi-%s', s.g),
       format('%s %s: yük testi için üretilmiş sentetik etkinlik.', t.topic, lower(k.kind)),
       format('%s alanında %s. Yük testi listesinin %s. kaydıdır; gerçek bir etkinlik değildir.', t.topic, lower(k.kind), s.g),
       k.category,
       s.format,
       CASE WHEN s.format <> 'online' THEN format('Kampüs Salonu %s', 1 + s.g % 12) END,
       CASE WHEN s.format <> 'online' THEN c.city END,
       CASE WHEN s.format <> 'in_person' THEN format('https://example.test/canli/%s', s.g) END,
       s.starts,
       s.starts + interval '2 hours',
       s.visibility,
       CASE WHEN s.visibility = 'university' THEN o.uni_id END,
       s.status,
       CASE WHEN s.past THEN s.starts - interval '14 days' ELSE now() - make_interval(days => 1 + s.g % 30) END,
       CASE WHEN s.status = 'cancelled' THEN now() END,
       CASE WHEN s.status = 'cancelled' THEN 'Yük testi: sentetik iptal' END,
       true, 'auto', 50 + s.g % 200
FROM (
  SELECT g,
         g % 5 = 0 AS past,
         CASE WHEN g % 5 = 0 THEN 'completed' WHEN g % 50 = 1 THEN 'cancelled' ELSE 'published' END AS status,
         CASE g % 20 WHEN 7 THEN 'university' WHEN 13 THEN 'members' ELSE 'public' END AS visibility,
         CASE g % 10 WHEN 3 THEN 'online' WHEN 6 THEN 'hybrid' ELSE 'in_person' END AS format,
         date_trunc('hour', now())
           + CASE WHEN g % 5 = 0 THEN -make_interval(days => 1 + g % 180) ELSE make_interval(days => 1 + g % 240) END
           + make_interval(hours => 9 + g % 10) AS starts
  FROM generate_series(1, :events) g
) s
JOIN lt_orgs o ON o.rn = 1 + (s.g * 7919) % :lt_org_count
JOIN lt_topics t ON t.i = 1 + s.g % 20
JOIN lt_kinds k ON k.i = 1 + (s.g / 20) % 10
JOIN lt_cities c ON c.i = 1 + (s.g * 31) % 10
ON CONFLICT ON CONSTRAINT events_org_slug_uq DO NOTHING;

COMMIT;

ANALYZE organizations;
ANALYZE clubs;
ANALYZE events;

SELECT 'bulk.sql: ' || (SELECT count(*) FROM events) || ' events (' ||
       (SELECT count(*) FROM events WHERE status = 'published' AND visibility = 'public' AND ends_at > now()) ||
       ' upcoming public), ' || (SELECT count(*) FROM clubs) || ' clubs, ' || :lt_org_count || ' organisers' AS summary;
