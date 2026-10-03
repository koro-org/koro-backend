-- 000110_seed_ledger_accounts.up.sql (02 §21.1). The twelve accounts of the
-- ledger. Model A (MVP) uses koro_bank, buyer_advances, seller_payable,
-- vat_output and platform_commission_revenue (02 §21.3 templates); the
-- others are ready for models B and C and for phase 2 (psp_clearing).
-- ON CONFLICT DO NOTHING: re-running never overwrites an account.
BEGIN;
SET LOCAL lock_timeout = '5s';
INSERT INTO ledger_accounts (code, name_tr, kind, per_payee) VALUES
  ('koro_bank',                   'Koro banka hesabı',                     'asset',     false),
  ('psp_clearing',                'Ödeme kuruluşu takas hesabı',           'asset',     false),
  ('buyer_advances',              'Alıcı avansları',                       'liability', false),
  ('seller_payable',              'Hakediş sahiplerine borçlar',           'liability', true),
  ('refund_payable',              'İade borçları',                         'liability', false),
  ('vat_output',                  'Hesaplanan KDV',                        'liability', false),
  ('vat_input',                   'İndirilecek KDV',                       'asset',     false),
  ('platform_commission_revenue', 'Platform komisyon gelirleri',           'revenue',   false),
  ('platform_sales_revenue',      'Platform hizmet satış gelirleri',       'revenue',   false),
  ('cost_of_services',            'Hizmet maliyetleri',                    'expense',   false),
  ('psp_fee_expense',             'Ödeme kuruluşu komisyon giderleri',     'expense',   false),
  ('bank_fee_expense',            'Banka masraf giderleri',                'expense',   false)
ON CONFLICT (code) DO NOTHING;
COMMIT;
