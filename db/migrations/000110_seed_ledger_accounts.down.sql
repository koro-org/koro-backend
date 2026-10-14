-- 000110_seed_ledger_accounts.down.sql
-- DATA LOSS: the seeded accounts (accounts added later stay).
BEGIN;
DELETE FROM ledger_accounts WHERE code IN (
  'koro_bank', 'psp_clearing', 'buyer_advances', 'seller_payable', 'refund_payable', 'vat_output', 'vat_input',
  'platform_commission_revenue', 'platform_sales_revenue', 'cost_of_services', 'psp_fee_expense', 'bank_fee_expense',
  'payee_receivable');
COMMIT;
