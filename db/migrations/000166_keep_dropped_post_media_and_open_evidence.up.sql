-- 000166_keep_dropped_post_media_and_open_evidence.up.sql (BE-158; the
-- security review of 2026-10-31, findings G-1 and G-2).
--   * post_dropped_media (G-1): an image an edit takes off a published post
--     is no longer deleted at once (BE-155 deletes it only for drafts and
--     posts in review). Its file stays attached to the post and this row
--     keeps what the post showed (alt text, size) and when it left, so a
--     reported image is still there when a moderator looks (adminGetReport)
--     and an edit cannot destroy the evidence of a report. Readers and the
--     author org never see it again (post_media no longer lists it). The
--     row goes with its post (ON DELETE CASCADE: the post_removed purge
--     deletes its file too) or 12 months after it was dropped through the
--     post_media_dropped policy below, never while a report on the post is
--     open or in review. file_id is the key: a file is dropped from one
--     post once (re-adding it to the post deletes the row first). No user
--     column: the editor is in the audit record (feed.post.update).
--   * order_lines.fulfillment_evidence_replaced_at (G-2): when the seller
--     last replaced a rejected delivery evidence (BE-154). The DTOs show it
--     to the buyer and finance.
--   * order_lines_evidence_open_guard: 000163's guard lets a rejected
--     evidence go for another file on any line; now that happens only while
--     the line is open — marked or disputed, the status unchanged by the
--     same statement, not in a payout — and stamps
--     fulfillment_evidence_replaced_at; the stamp moves with a replacement
--     only. The service also refuses a marked line whose dispute window has
--     passed and an order that no longer delivers (409 orders.invalid_state).
--     Raised as restrict_violation, CONSTRAINT order_lines_evidence_open.
BEGIN;
SET LOCAL lock_timeout = '5s';

CREATE TABLE post_dropped_media (
  file_id    uuid PRIMARY KEY CONSTRAINT post_dropped_media_file_id_fkey REFERENCES files (id),
  post_id    uuid NOT NULL CONSTRAINT post_dropped_media_post_id_fkey REFERENCES posts (id) ON DELETE CASCADE,
  alt_text   text CONSTRAINT post_dropped_media_alt_text_chk CHECK (char_length(alt_text) <= 300),
  width      integer CONSTRAINT post_dropped_media_width_chk CHECK (width > 0),
  height     integer CONSTRAINT post_dropped_media_height_chk CHECK (height > 0),
  dropped_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX post_dropped_media_post_idx ON post_dropped_media (post_id);
CREATE INDEX post_dropped_media_dropped_idx ON post_dropped_media (dropped_at);
-- SIUD like every hard-delete table (02 §0.5); UPDATE is the retention
-- sweep's row lock (SELECT … FOR UPDATE SKIP LOCKED), never an UPDATE.
GRANT SELECT, INSERT, UPDATE, DELETE ON post_dropped_media TO app_rw;

ALTER TABLE order_lines ADD COLUMN fulfillment_evidence_replaced_at timestamptz;
ALTER TABLE order_lines ADD CONSTRAINT order_lines_evidence_replaced_chk
  CHECK (fulfillment_evidence_replaced_at IS NULL OR fulfillment_evidence_file_id IS NOT NULL);

CREATE FUNCTION public.order_lines_evidence_open_guard() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.fulfillment_evidence_file_id IS NULL
     OR NEW.fulfillment_evidence_file_id IS NOT DISTINCT FROM OLD.fulfillment_evidence_file_id THEN
    -- the first evidence (the marking) or no change of it
    IF NEW.fulfillment_evidence_replaced_at IS DISTINCT FROM OLD.fulfillment_evidence_replaced_at THEN
      RAISE EXCEPTION 'UPDATE of order_lines.% is not allowed: fulfillment_evidence_replaced_at moves only with a replaced evidence', OLD.id
        USING ERRCODE = 'restrict_violation', CONSTRAINT = 'order_lines_evidence_open';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.fulfillment_status NOT IN ('marked', 'disputed') OR NEW.fulfillment_status IS DISTINCT FROM OLD.fulfillment_status
     OR OLD.payout_id IS NOT NULL OR OLD.paid_out_at IS NOT NULL
     OR NEW.fulfillment_evidence_replaced_at IS NULL
     OR NEW.fulfillment_evidence_replaced_at IS NOT DISTINCT FROM OLD.fulfillment_evidence_replaced_at THEN
    RAISE EXCEPTION 'UPDATE of order_lines.% is not allowed: the delivery evidence is replaced only on an open line (marked or disputed, not paid out), stamping fulfillment_evidence_replaced_at', OLD.id
      USING ERRCODE = 'restrict_violation', CONSTRAINT = 'order_lines_evidence_open';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER order_lines_evidence_open_guard
  BEFORE UPDATE OF fulfillment_evidence_file_id, fulfillment_evidence_replaced_at ON order_lines
  FOR EACH ROW EXECUTE FUNCTION public.order_lines_evidence_open_guard();

-- The dropped images' retention (unapproved like every seeded row, T-35).
INSERT INTO public.retention_policies (code, owner_module, target, retention, anchor, method, legal_basis, enabled, description_tr) VALUES
  ('post_media_dropped', 'feed', 'post_dropped_media', '12 months', 'closed_at', 'hard_delete', 'Moderasyon itirazı', true,
   'Yayımlanmış gönderiden düzenlemeyle çıkarılan görseller 12 ay sonra silinir; gönderide açık şikâyet varken beklenir.')
ON CONFLICT (code) DO NOTHING;
COMMIT;
