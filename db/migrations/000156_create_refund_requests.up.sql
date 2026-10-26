-- 000156_create_refund_requests.up.sql (BE-132; user decision Q-26,
-- 2026-10-26; 02 doc patch in the ticket). A buying company asks for a
-- refund of a paid order's line: an admin of the company opens the request
-- (the line, the amount in kuruş, the reason, an explanation, the IBAN the
-- money goes back to, an optional evidence file); it waits in finance's
-- queue ('open' → 'in_review'); a finance or platform admin accepts it — a
-- refund is opened in the same transaction (refunds: the four eyes and the
-- EFT stay the existing flow) — or rejects it with a reason; the buyer may
-- withdraw it while it waits. No money moves here: the request is a
-- promise, the refund is the money.
--   * files.purpose gains 'refund_request_evidence' (images + PDF, 10 MB,
--     org required = the buyer org; 03 k30: the buyer's company admin and
--     owner). The CHECK is replaced NOT VALID and validated afterwards, as
--     in 000137.
--   * 06 H-38: org_id is the order's buyer ((order_id, org_id) → orders),
--     seller_org_id the line's seller ((order_line_id, seller_org_id) →
--     order_lines, through the new order_lines_id_seller_uq) and the line
--     the order's ((order_line_id, order_id) → order_lines). order_no and
--     line_title are copies of frozen values (orders_frozen,
--     order_lines_frozen) so that the lists read no other module's table.
--   * The request goes with its order line, its order and its refund (ON
--     DELETE CASCADE): it is no money record of its own (the refund it opens
--     is), so the end-of-retention purge of financial_records
--     (financial_records_purge, 000123) takes it with the order aggregate it
--     belongs to without being rewritten (retention parent: orders). The
--     evidence file stays NO ACTION (as order_lines' delivery evidence; its
--     destruction follows T-280).
--   * refund_requests_line_pending_uq: one waiting request per line, so two
--     requests never promise the same money; the amounts are checked in Go
--     against what the line and the order still hold (open refunds count),
--     and again when the refund is opened.
--   * One CHECK per state (06 H-37): reviewed_by/at belong to in_review and
--     may stay on a decided or withdrawn request; decided_by/at to accepted
--     and rejected, refund_id to accepted, rejection_reason to rejected,
--     withdrawn_by/at to withdrawn. The decider and the reviewer are never
--     the requester (a staff member who is also an admin of the buying
--     company).
--   * Triggers: the request itself never changes (refund_requests_frozen);
--     the review, the decision and the withdrawal once set are final.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE files DROP CONSTRAINT files_purpose_chk;
ALTER TABLE files ADD CONSTRAINT files_purpose_chk CHECK (purpose IN (
  'avatar','cv','edu_document','org_logo','event_cover','post_media',
  'club_claim_evidence','party_document','bank_account_evidence',
  'transfer_receipt','payout_receipt','refund_receipt',
  'invoice_pdf','event_export','dsr_export','delivery_evidence',
  'refund_request_evidence')) NOT VALID;
ALTER TABLE files VALIDATE CONSTRAINT files_purpose_chk;
ALTER TABLE order_lines ADD CONSTRAINT order_lines_id_seller_uq UNIQUE (id, seller_org_id);

CREATE TABLE refund_requests (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  order_id          uuid NOT NULL,
  order_line_id     uuid NOT NULL,
  org_id            uuid NOT NULL,
  seller_org_id     uuid NOT NULL,
  order_no          text NOT NULL CONSTRAINT refund_requests_order_no_chk CHECK (order_no ~ '^KR[0-9A-HJKMNP-TV-Z]{8}$'),
  line_title        text NOT NULL CONSTRAINT refund_requests_line_title_chk CHECK (line_title <> ''),
  amount            bigint NOT NULL CONSTRAINT refund_requests_amount_chk CHECK (amount > 0),
  currency          text NOT NULL DEFAULT 'TRY' CONSTRAINT refund_requests_currency_chk CHECK (currency = 'TRY'),
  reason            text NOT NULL CONSTRAINT refund_requests_reason_chk CHECK (reason IN ('not_fulfilled','event_cancelled',
                      'offering_cancelled','duplicate_payment','other')),
  explanation       text NOT NULL CONSTRAINT refund_requests_explanation_chk CHECK (char_length(explanation) BETWEEN 10 AND 2000),
  target_iban       text NOT NULL CONSTRAINT refund_requests_target_iban_chk CHECK (public.koro_iban_tr_valid(target_iban)),
  evidence_file_id  uuid CONSTRAINT refund_requests_evidence_file_id_fkey REFERENCES files (id),
  status            text NOT NULL DEFAULT 'open' CONSTRAINT refund_requests_status_chk CHECK (status IN ('open','in_review',
                      'accepted','rejected','withdrawn')),
  requested_by      uuid NOT NULL CONSTRAINT refund_requests_requested_by_fkey REFERENCES users (id),
  reviewed_by       uuid CONSTRAINT refund_requests_reviewed_by_fkey REFERENCES users (id),
  reviewed_at       timestamptz,
  decided_by        uuid CONSTRAINT refund_requests_decided_by_fkey REFERENCES users (id),
  decided_at        timestamptz,
  rejection_reason  text CONSTRAINT refund_requests_rejection_reason_chk CHECK (char_length(rejection_reason) BETWEEN 1 AND 1000),
  refund_id         uuid CONSTRAINT refund_requests_refund_id_fkey REFERENCES refunds (id) ON DELETE CASCADE,
  withdrawn_by      uuid CONSTRAINT refund_requests_withdrawn_by_fkey REFERENCES users (id),
  withdrawn_at      timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT refund_requests_order_id_org_id_fkey FOREIGN KEY (order_id, org_id) REFERENCES orders (id, org_id) ON DELETE CASCADE,
  CONSTRAINT refund_requests_order_line_id_order_id_fkey FOREIGN KEY (order_line_id, order_id)
    REFERENCES order_lines (id, order_id) ON DELETE CASCADE,
  CONSTRAINT refund_requests_order_line_id_seller_org_id_fkey FOREIGN KEY (order_line_id, seller_org_id)
    REFERENCES order_lines (id, seller_org_id) ON DELETE CASCADE,
  -- the requester never decides (03 §6.1 spirit: no one decides their own money)
  CONSTRAINT refund_requests_review_eyes_chk CHECK (reviewed_by IS NULL OR reviewed_by <> requested_by),
  CONSTRAINT refund_requests_decision_eyes_chk CHECK (decided_by IS NULL OR decided_by <> requested_by),
  -- one CHECK per state (06 H-37)
  CONSTRAINT refund_requests_reviewed_chk CHECK ((reviewed_by IS NULL) = (reviewed_at IS NULL)
                                              AND (status <> 'in_review' OR reviewed_by IS NOT NULL)
                                              AND (status <> 'open' OR reviewed_by IS NULL)),
  CONSTRAINT refund_requests_decided_chk CHECK ((status IN ('accepted','rejected')) = (decided_by IS NOT NULL)
                                             AND (status IN ('accepted','rejected')) = (decided_at IS NOT NULL)),
  CONSTRAINT refund_requests_accepted_chk CHECK ((status = 'accepted') = (refund_id IS NOT NULL)),
  CONSTRAINT refund_requests_rejected_chk CHECK ((status = 'rejected') = (rejection_reason IS NOT NULL)),
  CONSTRAINT refund_requests_withdrawn_chk CHECK ((status = 'withdrawn') = (withdrawn_by IS NOT NULL)
                                              AND (status = 'withdrawn') = (withdrawn_at IS NOT NULL))
);
CREATE UNIQUE INDEX refund_requests_line_pending_uq ON refund_requests (order_line_id) WHERE status IN ('open','in_review');
CREATE UNIQUE INDEX refund_requests_refund_uq ON refund_requests (refund_id) WHERE refund_id IS NOT NULL;
CREATE INDEX refund_requests_org_idx ON refund_requests (org_id, created_at DESC, id DESC);
CREATE INDEX refund_requests_seller_idx ON refund_requests (seller_org_id, created_at DESC, id DESC);
CREATE INDEX refund_requests_order_idx ON refund_requests (order_id);
CREATE INDEX refund_requests_line_idx ON refund_requests (order_line_id);
CREATE INDEX refund_requests_pending_idx ON refund_requests (created_at, id) WHERE status IN ('open','in_review');
CREATE INDEX refund_requests_evidence_idx ON refund_requests (evidence_file_id) WHERE evidence_file_id IS NOT NULL;
CREATE TRIGGER refund_requests_touch_updated_at BEFORE UPDATE ON refund_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER refund_requests_initial_status BEFORE INSERT ON refund_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_initial_status('open');
CREATE TRIGGER refund_requests_frozen BEFORE UPDATE ON refund_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'order_id', 'order_line_id', 'org_id', 'seller_org_id', 'order_no',
    'line_title', 'amount', 'currency', 'reason', 'explanation', 'target_iban', 'evidence_file_id', 'requested_by', 'created_at');
CREATE TRIGGER refund_requests_review_frozen BEFORE UPDATE ON refund_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('reviewed_at', 'reviewed_by', 'reviewed_at');
CREATE TRIGGER refund_requests_decision_frozen BEFORE UPDATE ON refund_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('decided_at', 'status', 'decided_by', 'decided_at', 'rejection_reason',
    'refund_id', 'withdrawn_by', 'withdrawn_at');
CREATE TRIGGER refund_requests_withdrawal_frozen BEFORE UPDATE ON refund_requests
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('withdrawn_at', 'status', 'withdrawn_by', 'withdrawn_at', 'decided_by',
    'decided_at', 'rejection_reason', 'refund_id');
GRANT SELECT, INSERT, UPDATE ON refund_requests TO app_rw;
COMMIT;
