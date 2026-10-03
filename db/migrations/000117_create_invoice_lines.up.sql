-- 000117_create_invoice_lines.up.sql (02 §22.2). The lines of an invoice
-- (position 1..n), optionally tied to the order line they invoice. Never
-- deleted (financial_records).
-- 02 addition (H-20): vat_withheld_amount <= vat_amount.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE invoice_lines (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  invoice_id            uuid NOT NULL CONSTRAINT invoice_lines_invoice_id_fkey REFERENCES invoices (id),
  position              smallint NOT NULL CONSTRAINT invoice_lines_position_chk CHECK (position >= 1),
  order_line_id         uuid CONSTRAINT invoice_lines_order_line_id_fkey REFERENCES order_lines (id),
  description           text NOT NULL CONSTRAINT invoice_lines_description_chk CHECK (char_length(description) <= 300),
  quantity              integer NOT NULL CONSTRAINT invoice_lines_quantity_chk CHECK (quantity > 0),
  unit_price_amount     bigint NOT NULL CONSTRAINT invoice_lines_unit_price_amount_chk CHECK (unit_price_amount >= 0),
  net_amount            bigint NOT NULL CONSTRAINT invoice_lines_net_amount_chk CHECK (net_amount >= 0),
  vat_rate_bp           integer NOT NULL CONSTRAINT invoice_lines_vat_rate_bp_chk CHECK (vat_rate_bp BETWEEN 0 AND 10000),
  vat_amount            bigint NOT NULL CONSTRAINT invoice_lines_vat_amount_chk CHECK (vat_amount >= 0),
  vat_exemption_code    text,
  vat_withholding_code  text,
  vat_withheld_amount   bigint NOT NULL DEFAULT 0 CONSTRAINT invoice_lines_vat_withheld_amount_chk CHECK (vat_withheld_amount >= 0),
  total_amount          bigint NOT NULL CONSTRAINT invoice_lines_total_amount_chk CHECK (total_amount >= 0),
  CONSTRAINT invoice_lines_position_uq UNIQUE (invoice_id, position),
  CONSTRAINT invoice_lines_total_chk CHECK (total_amount = net_amount + vat_amount),
  CONSTRAINT invoice_lines_withheld_chk CHECK (vat_withheld_amount <= vat_amount)
);
CREATE INDEX invoice_lines_order_line_idx ON invoice_lines (order_line_id) WHERE order_line_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE ON invoice_lines TO app_rw;
COMMIT;
