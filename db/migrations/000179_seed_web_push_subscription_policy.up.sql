-- 000179_seed_web_push_subscription_policy.up.sql (02 §24.2; BE-177; user
-- decision T-409 of 2026-11-05). A browser subscription the browser has not
-- registered again for 90 days goes, as an unused mobile device token
-- (push_token, 000090): the web app registers its subscription again on
-- every start (web_push_subscriptions.last_seen_at), so a subscription
-- seen 90 days ago belongs to a browser no longer used. notify's sweeper
-- applies it through kvkk.retention_sweep (a dry run until the legal
-- approval); unapproved like every seeded row (T-35). ON CONFLICT DO
-- NOTHING: re-running never overwrites an edited row.
BEGIN;
SET LOCAL lock_timeout = '5s';
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('web_push_subscription', 'notify', 'web_push_subscriptions', '90 days', 'last_seen_at', 'hard_delete', 'Kullanılmayan tarayıcı', true,
   'Tarayıcının 90 gündür yeniden kaydetmediği web push abonelikleri silinir.')
ON CONFLICT (code) DO NOTHING;
COMMIT;
