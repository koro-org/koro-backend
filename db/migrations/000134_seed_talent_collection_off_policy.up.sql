-- 000134_seed_talent_collection_off_policy.up.sql (02 §24.2; BE-087, 00 T-324).
-- The destruction record category of the scores and cohorts talent.recompute
-- deletes while talent collection is off (talent.default_enabled=false):
-- without a purpose, the profiling output does not outlive the flag. A job
-- policy (talent.recompute): the flag decides, not an age, so the period is 0
-- and the job does not wait for the legal approval; unapproved like every
-- seeded row (T-35). ON CONFLICT DO NOTHING: re-running never overwrites an
-- edited row.
BEGIN;
SET LOCAL lock_timeout = '5s';
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('talent_collection_off', 'talent', 'talent.scores, talent.cohorts', '0', 'created_at', 'hard_delete', 'Veri minimizasyonu', true,
   'Talent puanı toplama kapalıyken (talent.default_enabled=false) skor ve kohort satırları her gece silinir.')
ON CONFLICT (code) DO NOTHING;
COMMIT;
