-- 000142_seed_talent_score_dropped_policy.up.sql (02 §24.2; BE-101, 00 T-342).
-- The destruction record category of the scores and cohorts talent.recompute
-- deletes with talent collection on, for a user it no longer scores: no ledger
-- row left in lookback_days, no student profile or a minor (02 §15.5, §15.6).
-- A job policy (talent.recompute): the run decides, not an age, so the period
-- is 0 and the job does not wait for the legal approval; unapproved like every
-- seeded row (T-35). ON CONFLICT DO NOTHING: re-running never overwrites an
-- edited row.
BEGIN;
SET LOCAL lock_timeout = '5s';
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('talent_score_dropped', 'talent', 'talent.scores, talent.cohorts', '0', 'created_at', 'hard_delete', 'Veri minimizasyonu', true,
   'Talent puanı toplama açıkken puanlama penceresinde defter satırı kalmayan, öğrenci profili olmayan ya da reşit olmayan kullanıcının skor ve kohort satırları gece koşusunda silinir.')
ON CONFLICT (code) DO NOTHING;
COMMIT;
