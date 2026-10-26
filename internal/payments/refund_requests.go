package payments

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Refund requests (BE-132; user decision Q-26, 2026-10-26): a buying
// company asks for its money back, finance decides, an accepted request
// opens a refund. The money still leaves only through the refund flow of
// refunds.go (its four eyes, its EFT, its ledger).
//
//	1. request  (createOrderRefundRequest, the buyer's company admin or
//	            owner; Idempotency-Key) → open: one line of a paid order,
//	            the amount, a reason, an explanation, the IBAN, an optional
//	            refund_request_evidence upload of the buyer org;
//	2. review   (adminReviewRefundRequest, pad/fin) → in_review: finance
//	            took it (the buyer is told);
//	3. decision (adminAcceptRefundRequest, pad/fin; Idempotency-Key) →
//	            accepted with the refund it opened in the same transaction
//	            (requested: another finance admin approves it, 03 §6.1); or
//	            adminRejectRefundRequest → rejected with a reason. Neither by
//	            the requester (a staff member who is also an admin of the
//	            buying company): authz four eyes + the eyes CHECKs.
//	   withdrawal (withdrawRefundRequest, the buyer's admin) → withdrawn,
//	            while it waits (open or in_review).
//
// Which lines (option): an order paid in full (paid, in_fulfilment,
// completed, disputed, partially_refunded; not one refunded in full — an
// unpaid order's money is finance's refunds due), a line not cancelled
// (a cancelled line is refunded without a request: refunds due), at most
// one waiting request per line, and within the window: an undelivered line
// any time, a delivered one until fulfilled_at + PAYMENTS_REFUND_REQUEST_WINDOW
// (30 days by default; a platform-native line is delivered at payment).
//
// How much (requestable, Go, kuruş): what Koro holds of the line (its buyer
// fee only in model C) less what was refunded and what open refunds
// promise (lineLeft), and no more than the order still holds less the
// other lines' waiting requests (orderLeft). The acceptance checks again
// under the payment's lock (createRefund: the payment, the order, the
// line, the payout) and is the authoritative one.
//
// Who sees what: the buyer's members everything of their requests (the
// IBAN masked); the seller's staff the requests on their lines without the
// IBAN or the buyer's user; finance everything (the full IBAN goes on to
// the refund, revealed there, BE-129). The evidence is downloaded by the
// parties only, through a 5-minute URL, every download audited.

// ---- shared

// orgSummary returns the org of an org-surface request (404 when unknown);
// without the org directory the request fails closed.
func (s *service) orgSummary(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error) {
	if s.d.Orgs == nil {
		return orgs.OrgSummary{}, ErrUnavailable
	}
	o, err := s.d.Orgs.Get(ctx, orgID)
	if errors.Is(err, orgs.ErrNotFound) {
		return orgs.OrgSummary{}, ErrNotFound
	}
	return o, err
}

// buyerOrder returns the order of the buyer org (404 for another org's
// order: it is never probed).
func (s *service) buyerOrder(ctx context.Context, orgID, orderID uuid.UUID) (orders.PayableOrder, error) {
	o, err := s.d.Orders.ForPayment(ctx, orderID)
	switch {
	case errors.Is(err, orders.ErrNotFound):
		return orders.PayableOrder{}, ErrNotFound
	case err != nil:
		return orders.PayableOrder{}, err
	case o.OrgID != orgID:
		return orders.PayableOrder{}, ErrNotFound
	}
	return o, nil
}

// requestAudit is the audit record of a request action: IDs, the amount,
// the codes and the IBAN masked; free text only as the decision's reason.
func requestAudit(a authz.Actor, act authz.Action, r RefundRequest, reason string, meta map[string]any) audit.Event {
	id, org := r.ID, r.OrgID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["status"] = string(r.Status)
	meta["amount"] = r.Amount
	meta["reason_code"] = r.Reason
	meta["order_id"] = r.OrderID.String()
	meta["order_line_id"] = r.OrderLineID.String()
	meta["seller_org_id"] = r.SellerOrgID.String()
	meta["target_iban_masked"] = r.MaskedIBAN()
	meta["has_evidence"] = r.EvidenceFileID != nil
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: authz.TypeRefundRequest, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// deadline is the end of line l's request window: its delivery plus the
// window; nil while it is undelivered.
func (s *service) deadline(l orders.Line) *time.Time {
	if l.FulfilledAt == nil || l.FulfillmentStatus != orders.FulfillFulfilled {
		return nil
	}
	d := l.FulfilledAt.UTC().Add(s.d.Config.RefundRequestWindow)
	return &d
}

// requestable is what a request of line l may ask (kuruş): what Koro still
// holds of the line (lineLeft: refunds and open refunds count) within what
// the order still holds less the waiting requests of its other lines.
func requestable(m orderMoney, reqs []RefundRequest, l orders.Line) int64 {
	var others int64
	for _, r := range reqs {
		if r.Status.Pending() && r.OrderLineID != l.ID {
			others += r.Amount
		}
	}
	return max(min(m.lineLeft(l, uuid.Nil()), m.orderLeft(uuid.Nil())-others), 0)
}

// option is what the buyer may still ask of line l now (see the comment
// of this file).
func (s *service) option(m orderMoney, reqs []RefundRequest, l orders.Line, now time.Time) RefundOption {
	o := RefundOption{LineID: l.ID, Title: l.TitleSnapshot, SellerOrgID: l.SellerOrgID, Deadline: s.deadline(l),
		Refundable: requestable(m, reqs, l)}
	for _, r := range reqs {
		if r.Status.Pending() && r.OrderLineID == l.ID {
			id := r.ID
			o.OpenRequest = &id
		}
	}
	st := m.order.Status
	switch {
	case !st.PaidState() || st == orders.StatusRefunded:
		o.Blocked = BlockedOrderState
	case l.FulfillmentStatus == orders.FulfillCancelled:
		o.Blocked = BlockedLineState
	case o.OpenRequest != nil:
		o.Blocked = BlockedOpenRequest
	case o.Deadline != nil && !now.Before(*o.Deadline):
		o.Blocked = BlockedWindow
	case o.Refundable == 0:
		o.Blocked = BlockedNothingLeft
	}
	if o.Blocked != "" {
		o.Refundable = 0
	}
	return o
}

// blockedErr is the answer to a request of a blocked line.
func blockedErr(o RefundOption, st orders.Status) error {
	switch o.Blocked {
	case BlockedOrderState:
		if !st.PaidState() {
			return orders.ErrOrderNotPaid
		}
		return orders.ErrInvalidState.WithFields(fieldErrOf("path.order_id", "the order is refunded in full"))
	case BlockedLineState:
		return orders.ErrInvalidState.WithFields(fieldErrOf("body.order_line_id",
			"the line is cancelled: finance refunds it without a request"))
	case BlockedOpenRequest:
		return ErrRefundRequestOpen
	case BlockedWindow:
		return ErrRefundRequestWindowClosed
	case BlockedNothingLeft:
		return exceeds("Koro holds nothing more of the line for a refund")
	}
	return nil
}

// audienceOf is whose view of r a reader authorised to read it gets: the
// buyer's members, platform staff, or else the seller's staff.
func audienceOf(a authz.Actor, r RefundRequest) RequestAudience {
	switch {
	case a.HasOrgRole(r.OrgID, authz.MemberRecruiter, authz.MemberCompanyAdmin, authz.MemberCompanyOwner):
		return AudienceBuyer
	case a.HasAnyPlatformRole():
		return AudienceStaff
	}
	return AudienceSeller
}

// views adds the org names to rows (one directory read for the page).
func (s *service) views(ctx context.Context, rows []RefundRequest, aud RequestAudience) ([]RefundRequestView, error) {
	if s.d.Orgs == nil {
		return nil, ErrUnavailable
	}
	ids := make([]uuid.UUID, 0, 2*len(rows))
	for _, r := range rows {
		ids = append(ids, r.OrgID, r.SellerOrgID)
	}
	slices.SortFunc(ids, compareUUID)
	names, err := s.d.Orgs.GetMany(ctx, slices.Compact(ids))
	if err != nil {
		return nil, err
	}
	out := make([]RefundRequestView, len(rows))
	for i, r := range rows {
		out[i] = RefundRequestView{Request: r, BuyerName: names[r.OrgID].Name, SellerName: names[r.SellerOrgID].Name, Audience: aud}
	}
	return out, nil
}

// view reads one request back as aud sees it, with the refund it opened.
func (s *service) view(ctx context.Context, id uuid.UUID, aud RequestAudience) (RefundRequestView, error) {
	r, err := s.d.Store.GetRefundRequest(ctx, id, false)
	if err != nil {
		return RefundRequestView{}, err
	}
	vs, err := s.views(ctx, []RefundRequest{r}, aud)
	if err != nil {
		return RefundRequestView{}, err
	}
	v := vs[0]
	if r.RefundID != nil {
		ref, err := s.d.Store.GetRefund(ctx, *r.RefundID, false)
		if err != nil {
			return RefundRequestView{}, err
		}
		v.Refund = &ref
	}
	return v, nil
}

// ---- the buyer

// requestDraft is a validated request (and its idempotency hash input).
type requestDraft struct {
	LineID      uuid.UUID
	Amount      int64
	Reason      string
	Explanation string
	TargetIBAN  string
	Evidence    *uuid.UUID
}

// cleanRequest validates the body of createOrderRefundRequest (422).
func cleanRequest(in RefundRequestInput) (requestDraft, error) {
	d := requestDraft{LineID: in.OrderLineID, Amount: in.Amount, Reason: in.Reason, Evidence: in.EvidenceFileID}
	switch {
	case d.LineID == uuid.Nil():
		return requestDraft{}, problem.Field("body.order_line_id", "is required")
	case d.Amount <= 0:
		return requestDraft{}, problem.Field("body.amount", "must be a positive amount in kuruş")
	case !slices.Contains(RequestReasons(), d.Reason):
		return requestDraft{}, problem.Field("body.reason", "is not a refund request reason")
	case d.Evidence != nil && *d.Evidence == uuid.Nil():
		return requestDraft{}, problem.Field("body.evidence_file_id", "must be a file ID")
	case in.TargetIBAN == "":
		return requestDraft{}, problem.Field("body.target_iban", "is required: the IBAN the money goes back to")
	}
	var err error
	if d.Explanation, err = cleanText("body.explanation", in.Explanation, minExplanation, maxExplanation); err != nil {
		return requestDraft{}, err
	}
	if d.TargetIBAN, err = cleanIBAN("body.target_iban", in.TargetIBAN); err != nil {
		return requestDraft{}, err
	}
	return d, nil
}

// RefundOptions implements Service (payments.refund_request.list on the
// buyer org): the org (404) → authz (404/403) → the order of the org (404)
// → every line with what may still be asked of it, and the order's
// requests (newest first).
func (s *service) RefundOptions(ctx context.Context, a authz.Actor, orgID, orderID uuid.UUID) (RefundOptions, error) {
	org, err := s.orgSummary(ctx, orgID)
	if err != nil {
		return RefundOptions{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestList, authz.OrgResource(org.ID, string(org.Kind), string(org.Status))); err != nil {
		return RefundOptions{}, err
	}
	if _, err := s.buyerOrder(ctx, org.ID, orderID); err != nil {
		return RefundOptions{}, err
	}
	o, lines, err := s.d.Orders.ForBilling(ctx, orderID)
	if err != nil {
		return RefundOptions{}, err
	}
	m, err := s.orderMoney(ctx, orderID)
	if err != nil {
		return RefundOptions{}, err
	}
	reqs, err := s.d.Store.RefundRequestsOfOrder(ctx, orderID)
	if err != nil {
		return RefundOptions{}, err
	}
	now := s.now()
	out := RefundOptions{OrderID: o.ID, OrderNo: o.OrderNo, Status: string(o.Status), Currency: o.Currency,
		Lines: make([]RefundOption, 0, len(lines))}
	for _, l := range lines {
		out.Lines = append(out.Lines, s.option(m, reqs, l, now))
	}
	newest := slices.Clone(reqs)
	slices.Reverse(newest)
	if out.Requests, err = s.views(ctx, newest, AudienceBuyer); err != nil {
		return RefundOptions{}, err
	}
	return out, nil
}

// CreateRefundRequest implements Service (Idempotency-Key): the org (404) →
// authz (cma of a company: 404/403) → the body (422) → the order of the org
// (404) → its line (422) → in one transaction with the key (06 H-5): the
// line's option (409 orders.order_not_paid, orders.invalid_state,
// payments.refund_request_open, payments.refund_request_window_closed) and
// the amount (422 orders.refund_exceeds_paid) → the evidence (422; 409
// files.*) → the request (open), the evidence attached → audit → the
// notices after commit.
func (s *service) CreateRefundRequest(ctx context.Context, a authz.Actor, orgID, orderID uuid.UUID, in RefundRequestInput) (RefundRequestView, error) {
	org, err := s.orgSummary(ctx, orgID)
	if err != nil {
		return RefundRequestView{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestCreate, authz.OrgResource(org.ID, string(org.Kind), string(org.Status))); err != nil {
		return RefundRequestView{}, err
	}
	d, err := cleanRequest(in)
	if err != nil {
		return RefundRequestView{}, err
	}
	o, err := s.buyerOrder(ctx, org.ID, orderID)
	if err != nil {
		return RefundRequestView{}, err
	}
	l, err := s.d.Orders.GetLine(ctx, d.LineID)
	switch {
	case errors.Is(err, orders.ErrNotFound) || err == nil && l.OrderID != o.ID:
		return RefundRequestView{}, problem.Field("body.order_line_id", "is not a line of the order")
	case err != nil:
		return RefundRequestView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op         string
		Org, Order uuid.UUID
		D          requestDraft
	}{"create_refund_request", org.ID, o.ID, d})
	if err != nil {
		return RefundRequestView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		return s.createRequest(ctx, a, l, d)
	})
	if err != nil {
		return RefundRequestView{}, err
	}
	return s.view(ctx, id, AudienceBuyer)
}

func (s *service) createRequest(ctx context.Context, a authz.Actor, line orders.Line, d requestDraft) (uuid.UUID, error) {
	l, err := s.d.Orders.GetLine(ctx, line.ID) // its state in the key's transaction
	if err != nil {
		return uuid.Nil(), err
	}
	m, err := s.orderMoney(ctx, l.OrderID)
	if err != nil {
		return uuid.Nil(), err
	}
	reqs, err := s.d.Store.RefundRequestsOfOrder(ctx, l.OrderID)
	if err != nil {
		return uuid.Nil(), err
	}
	opt := s.option(m, reqs, l, s.now())
	if err := blockedErr(opt, m.order.Status); err != nil {
		return uuid.Nil(), err
	}
	if d.Amount > opt.Refundable {
		return uuid.Nil(), exceeds("exceeds what the line still holds for a refund (refunds and waiting requests count)")
	}
	r := RefundRequest{
		ID: uuid.NewV7(), OrderID: m.order.ID, OrderLineID: l.ID, OrgID: m.order.OrgID, SellerOrgID: l.SellerOrgID,
		OrderNo: m.order.OrderNo, LineTitle: l.TitleSnapshot, Amount: d.Amount, Currency: Currency, Reason: d.Reason,
		Explanation: d.Explanation, TargetIBAN: d.TargetIBAN, EvidenceFileID: d.Evidence, Status: RequestOpen, RequestedBy: a.UserID,
	}
	if d.Evidence != nil {
		// Inside the key's transaction: a replay never sees its own attached file.
		if err := s.checkRequestEvidence(ctx, r.OrgID, *d.Evidence); err != nil {
			return uuid.Nil(), err
		}
	}
	if err := s.d.Store.InsertRefundRequest(ctx, r); err != nil {
		return uuid.Nil(), err
	}
	if d.Evidence != nil {
		if err := s.d.Files.Attach(ctx, a, *d.Evidence, requestOwner(r.ID)); err != nil {
			if errors.Is(err, files.ErrNotFound) {
				return uuid.Nil(), evidenceField()
			}
			return uuid.Nil(), err
		}
	}
	if err := s.d.Audit.Log(ctx, requestAudit(a, authz.PaymentsRefundRequestCreate, r, "", nil)); err != nil {
		return uuid.Nil(), err
	}
	s.notifyRequest(ctx, r.ID, RequestOpen)
	s.logInfo(ctx, "refund request opened", slog.String("refund_request_id", r.ID.String()),
		slog.String("order_line_id", l.ID.String()), slog.Int64("amount", r.Amount))
	return r.ID, nil
}

// evidenceField is the 422 of an evidence file that is not the buyer's.
func evidenceField() error {
	return problem.Field("body.evidence_file_id", "must be a refund_request_evidence upload of the buying company")
}

// checkRequestEvidence accepts a completed refund_request_evidence upload of
// the buyer org (06 H-38: the org of the order, never the request's) that
// no record holds yet; another org's file reads as an unknown one.
func (s *service) checkRequestEvidence(ctx context.Context, buyer, fileID uuid.UUID) error {
	if s.d.Files == nil {
		return ErrUnavailable
	}
	f, err := s.d.Files.Get(ctx, fileID)
	if err == nil {
		err = f.CheckOrgUpload(buyer, requestOwner(uuid.Nil()))
	}
	switch {
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrNotOrgUpload):
		return evidenceField()
	case errors.Is(err, files.ErrUploadNotActive):
		return problem.Field("body.evidence_file_id", "must be a completed upload")
	}
	return err
}

// ListOrgRefundRequests implements Service (the buyer org's requests,
// newest first).
func (s *service) ListOrgRefundRequests(ctx context.Context, a authz.Actor, orgID uuid.UUID, q RefundRequestQuery) (RefundRequestPage, error) {
	org, err := s.orgSummary(ctx, orgID)
	if err != nil {
		return RefundRequestPage{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestList, authz.OrgResource(org.ID, string(org.Kind), string(org.Status))); err != nil {
		return RefundRequestPage{}, err
	}
	return s.listRequests(ctx, RefundRequestFilter{OrgID: &org.ID}, q, AudienceBuyer)
}

// ListSalesRefundRequests implements Service (the requests on the seller
// org's lines, newest first; the seller's view).
func (s *service) ListSalesRefundRequests(ctx context.Context, a authz.Actor, orgID uuid.UUID, q RefundRequestQuery) (RefundRequestPage, error) {
	org, err := s.orgSummary(ctx, orgID)
	if err != nil {
		return RefundRequestPage{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestListSales, authz.OrgResource(org.ID, string(org.Kind), string(org.Status))); err != nil {
		return RefundRequestPage{}, err
	}
	return s.listRequests(ctx, RefundRequestFilter{SellerOrgID: &org.ID}, q, AudienceSeller)
}

// AdminListRefundRequests implements Service (finance's queue: every
// request, oldest first).
func (s *service) AdminListRefundRequests(ctx context.Context, a authz.Actor, q RefundRequestQuery) (RefundRequestPage, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestListAdmin, adminSurface); err != nil {
		return RefundRequestPage{}, err
	}
	return s.listRequests(ctx, RefundRequestFilter{Oldest: true}, q, AudienceStaff)
}

// listRequests is a page of f narrowed by q (422 for a malformed query).
func (s *service) listRequests(ctx context.Context, f RefundRequestFilter, q RefundRequestQuery, aud RequestAudience) (RefundRequestPage, error) {
	if q.invalid != nil {
		return RefundRequestPage{}, q.invalid
	}
	if q.Status != "" && !q.Status.Valid() {
		return RefundRequestPage{}, problem.Field("query.status", "is not a refund request status")
	}
	f.Status, f.Pending, f.OrderID = q.Status, q.Pending, q.OrderID
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListRefundRequests(ctx, f, q.After, limit+1)
	if err != nil {
		return RefundRequestPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	items, err := s.views(ctx, rows, aud)
	if err != nil {
		return RefundRequestPage{}, err
	}
	out := RefundRequestPage{Items: items}
	if more {
		last := rows[len(rows)-1]
		out.Next = &TimeCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// GetRefundRequest implements Service: the request (404) → authz (404 for
// whoever sees neither its buyer nor its seller, 403 without the cell) →
// the reader's view.
func (s *service) GetRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID) (RefundRequestView, error) {
	r, err := s.d.Store.GetRefundRequest(ctx, id, false)
	if err != nil {
		return RefundRequestView{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestRead, requestResource(r)); err != nil {
		return RefundRequestView{}, err
	}
	return s.view(ctx, r.ID, audienceOf(a, r))
}

// WithdrawRefundRequest implements Service: the request (404) → authz (the
// buyer's admin) → in one transaction (Idempotency-Key optional): a
// waiting request only (409 payments.invalid_state) → withdrawn → audit →
// the notices after commit.
func (s *service) WithdrawRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID) (RefundRequestView, error) {
	r, err := s.d.Store.GetRefundRequest(ctx, id, false)
	if err != nil {
		return RefundRequestView{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestWithdraw, requestResource(r)); err != nil {
		return RefundRequestView{}, err
	}
	err = s.transitionRequest(ctx, "withdraw_refund_request", id, nil, func(ctx context.Context, cur RefundRequest) (RefundRequest, error) {
		if !cur.Status.Pending() {
			return RefundRequest{}, ErrInvalidState
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.WithdrawnBy, next.WithdrawnAt = RequestWithdrawn, &by, &now
		stored, err := s.d.Store.UpdateRefundRequest(ctx, next, cur.Status)
		if err != nil {
			return RefundRequest{}, err
		}
		s.notifyRequest(ctx, stored.ID, RequestWithdrawn)
		return stored, s.d.Audit.Log(ctx, requestAudit(a, authz.PaymentsRefundRequestWithdraw, stored, "",
			map[string]any{"from": string(cur.Status)}))
	})
	if err != nil {
		return RefundRequestView{}, err
	}
	return s.view(ctx, id, AudienceBuyer)
}

// RefundRequestEvidence implements Service: the request (404) → authz (the
// parties: 404/403) → its evidence (404 without one) → a 5-minute URL of
// the clean file (409 files.not_clean) → audit (every download) before
// the URL leaves.
func (s *service) RefundRequestEvidence(ctx context.Context, a authz.Actor, id uuid.UUID) (files.Download, error) {
	r, err := s.d.Store.GetRefundRequest(ctx, id, false)
	if err != nil {
		return files.Download{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestReadEvidence, requestResource(r)); err != nil {
		return files.Download{}, err
	}
	if r.EvidenceFileID == nil {
		return files.Download{}, ErrNotFound
	}
	if s.d.Files == nil {
		return files.Download{}, ErrUnavailable
	}
	dl, err := s.d.Files.PresignGet(ctx, *r.EvidenceFileID, receiptTTL)
	if err != nil {
		return files.Download{}, err
	}
	meta := map[string]any{"operation": "download_evidence", "evidence_file_id": r.EvidenceFileID.String(),
		"ttl_seconds": int(receiptTTL.Seconds()), "audience": string(audienceOf(a, r))}
	if err := s.d.Audit.Log(ctx, requestAudit(a, authz.PaymentsRefundRequestReadEvidence, r, "", meta)); err != nil {
		return files.Download{}, err
	}
	return dl, nil
}

// ---- finance

// decided reads the request of a decision after the "403 before reading"
// gate: the request (404) → the eyes on it (403 authz.four_eyes_violation
// for its requester).
func (s *service) decided(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) (RefundRequest, error) {
	if err := authz.Can(ctx, a, act, adminSurface); err != nil {
		return RefundRequest{}, err
	}
	r, err := s.d.Store.GetRefundRequest(ctx, id, false)
	if err != nil {
		return RefundRequest{}, err
	}
	return r, authz.Can(ctx, a, act, requestResource(r))
}

// AdminGetRefundRequest implements Service (payments.refund_request.list_admin):
// the request, what its line may still give back now and the order's
// payments that can fund it.
func (s *service) AdminGetRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID) (RefundRequestDetail, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestListAdmin, adminSurface); err != nil {
		return RefundRequestDetail{}, err
	}
	v, err := s.view(ctx, id, AudienceStaff)
	if err != nil {
		return RefundRequestDetail{}, err
	}
	r := v.Request
	m, err := s.orderMoney(ctx, r.OrderID)
	if err != nil {
		return RefundRequestDetail{}, err
	}
	l, err := s.d.Orders.GetLine(ctx, r.OrderLineID)
	if err != nil {
		return RefundRequestDetail{}, err
	}
	reqs, err := s.d.Store.RefundRequestsOfOrder(ctx, r.OrderID)
	if err != nil {
		return RefundRequestDetail{}, err
	}
	out := RefundRequestDetail{RefundRequestView: v, LineRefundable: requestable(m, reqs, l), Payments: []FundingPayment{}}
	for _, p := range m.payments {
		if left := m.paymentLeft(p, uuid.Nil()); p.refundable() && left > 0 {
			out.Payments = append(out.Payments, FundingPayment{PaymentID: p.ID, Available: left})
		}
	}
	return out, nil
}

// ReviewRefundRequest implements Service: the gate (403 before reading) →
// the request (404) → the eyes (403) → in one transaction (Idempotency-Key
// optional): an open request only (409 payments.invalid_state) →
// in_review by the actor → audit → the buyer is told after commit.
func (s *service) ReviewRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID) (RefundRequestView, error) {
	if _, err := s.decided(ctx, a, authz.PaymentsRefundRequestReview, id); err != nil {
		return RefundRequestView{}, err
	}
	err := s.transitionRequest(ctx, "review_refund_request", id, nil, func(ctx context.Context, cur RefundRequest) (RefundRequest, error) {
		if cur.Status != RequestOpen {
			return RefundRequest{}, ErrInvalidState
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.ReviewedBy, next.ReviewedAt = RequestInReview, &by, &now
		stored, err := s.d.Store.UpdateRefundRequest(ctx, next, RequestOpen)
		if err != nil {
			return RefundRequest{}, err
		}
		s.notifyRequest(ctx, stored.ID, RequestInReview)
		return stored, s.d.Audit.Log(ctx, requestAudit(a, authz.PaymentsRefundRequestReview, stored, "", nil))
	})
	if err != nil {
		return RefundRequestView{}, err
	}
	return s.view(ctx, id, AudienceStaff)
}

// RejectRefundRequest implements Service: the gate (403) → the reason
// (422) → the request (404) → the eyes (403) → in one transaction: a
// waiting request only (409 payments.invalid_state) → rejected with the
// reason → audit → the buyer and the seller are told after commit.
func (s *service) RejectRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (RefundRequestView, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestReject, adminSurface); err != nil {
		return RefundRequestView{}, err
	}
	why, err := cleanText("body.reason", reason, 1, maxRequestDecision)
	if err != nil {
		return RefundRequestView{}, err
	}
	if _, err := s.decided(ctx, a, authz.PaymentsRefundRequestReject, id); err != nil {
		return RefundRequestView{}, err
	}
	err = s.transitionRequest(ctx, "reject_refund_request", id, why, func(ctx context.Context, cur RefundRequest) (RefundRequest, error) {
		if !cur.Status.Pending() {
			return RefundRequest{}, ErrInvalidState
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.DecidedBy, next.DecidedAt, next.RejectionReason = RequestRejected, &by, &now, &why
		stored, err := s.d.Store.UpdateRefundRequest(ctx, next, cur.Status)
		if err != nil {
			return RefundRequest{}, err
		}
		s.notifyRequest(ctx, stored.ID, RequestRejected)
		return stored, s.d.Audit.Log(ctx, requestAudit(a, authz.PaymentsRefundRequestReject, stored, why, nil))
	})
	if err != nil {
		return RefundRequestView{}, err
	}
	return s.view(ctx, id, AudienceStaff)
}

// acceptDraft is a validated acceptance (and its idempotency hash input).
type acceptDraft struct {
	PaymentID    *uuid.UUID
	TargetIBAN   string
	RefundReason string
	Note         string
}

// cleanAccept validates the body of adminAcceptRefundRequest (422).
func cleanAccept(in AcceptInput) (acceptDraft, error) {
	d := acceptDraft{PaymentID: in.PaymentID, RefundReason: in.RefundReason}
	switch {
	case d.PaymentID != nil && *d.PaymentID == uuid.Nil():
		return acceptDraft{}, problem.Field("body.payment_id", "must be a payment ID")
	case d.RefundReason != "" && (!slices.Contains(RefundReasons(), d.RefundReason) || d.RefundReason == RefundReasonOverpayment):
		return acceptDraft{}, problem.Field("body.refund_reason", "is not a refund reason of an order line")
	}
	var err error
	if d.Note, err = cleanText("body.note", in.Note, 0, maxRefundNote); err != nil {
		return acceptDraft{}, err
	}
	if in.TargetIBAN != "" {
		if d.TargetIBAN, err = cleanIBAN("body.target_iban", in.TargetIBAN); err != nil {
			return acceptDraft{}, err
		}
	}
	return d, nil
}

// AcceptRefundRequest implements Service (Idempotency-Key): the gate (403
// before reading) → the body (422) → the request (404) → the eyes (403) →
// in ONE transaction with the key (06 H-5):
//
//  1. the request locked: a waiting one only (409 payments.invalid_state);
//  2. the payment: the one given (422 when it is not the order's) or the
//     order's first captured transfer that still holds the whole amount
//     (422 body.payment_id when none does);
//  3. the refund, through adminCreateRefund's path (createRefund: the
//     payment locked, every limit again with the open refunds, a line in an
//     open payout 409 payments.line_in_payout; requested by the actor, so
//     another finance admin approves it; its own audit record and notice);
//  4. the request accepted with the refund → audit → the buyer and the
//     seller are told after commit.
//
// Any failure rolls everything back: the request still waits.
func (s *service) AcceptRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID, in AcceptInput) (RefundRequestView, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestAccept, adminSurface); err != nil {
		return RefundRequestView{}, err
	}
	d, err := cleanAccept(in)
	if err != nil {
		return RefundRequestView{}, err
	}
	if _, err := s.decided(ctx, a, authz.PaymentsRefundRequestAccept, id); err != nil {
		return RefundRequestView{}, err
	}
	err = s.transitionRequest(ctx, "accept_refund_request", id, d, func(ctx context.Context, cur RefundRequest) (RefundRequest, error) {
		if !cur.Status.Pending() {
			return RefundRequest{}, ErrInvalidState
		}
		pay, err := s.fundingPayment(ctx, cur, d.PaymentID)
		if err != nil {
			return RefundRequest{}, err
		}
		rd := refundDraft{PaymentID: pay, LineID: &cur.OrderLineID, Amount: cur.Amount, Reason: orDefault(d.RefundReason, cur.Reason),
			Note: d.Note, Method: RefundMethodBankEFT, TargetIBAN: orDefault(d.TargetIBAN, cur.TargetIBAN)}
		refundID, err := s.createRefund(ctx, a, rd, map[string]any{"via": "refund_request", "refund_request_id": cur.ID.String()})
		if err != nil {
			return RefundRequest{}, err
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.DecidedBy, next.DecidedAt, next.RefundID = RequestAccepted, &by, &now, &refundID
		stored, err := s.d.Store.UpdateRefundRequest(ctx, next, cur.Status)
		if err != nil {
			return RefundRequest{}, err
		}
		s.notifyRequest(ctx, stored.ID, RequestAccepted)
		return stored, s.d.Audit.Log(ctx, requestAudit(a, authz.PaymentsRefundRequestAccept, stored, "", map[string]any{
			"refund_id": refundID.String(), "payment_id": pay.String(), "refund_reason": rd.Reason,
			"iban_changed": d.TargetIBAN != "" && d.TargetIBAN != cur.TargetIBAN,
		}))
	})
	if err != nil {
		return RefundRequestView{}, err
	}
	return s.view(ctx, id, AudienceStaff)
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// fundingPayment is the payment an accepted request's refund draws from:
// the one finance chose (a payment of the request's order) or the order's
// oldest captured bank transfer that still holds the whole amount.
func (s *service) fundingPayment(ctx context.Context, r RefundRequest, chosen *uuid.UUID) (uuid.UUID, error) {
	if chosen != nil {
		p, err := s.d.Store.GetPayment(ctx, *chosen, false)
		switch {
		case isNotFound(err):
			return uuid.Nil(), problem.Field("body.payment_id", "is not a payment")
		case err != nil:
			return uuid.Nil(), err
		case p.OrderID != r.OrderID:
			return uuid.Nil(), problem.Field("body.payment_id", "is not a payment of the request's order")
		}
		return p.ID, nil
	}
	m, err := s.orderMoney(ctx, r.OrderID)
	if err != nil {
		return uuid.Nil(), err
	}
	for _, p := range m.payments {
		if p.refundable() && m.paymentLeft(p, uuid.Nil()) >= r.Amount {
			return p.ID, nil
		}
	}
	return uuid.Nil(), problem.Field("body.payment_id",
		"is required: no captured payment of the order holds the whole amount (choose one, or refund by hand)")
}

// transitionRequest runs apply on the locked request in one transaction
// with the Idempotency-Key (op and in name the change in its hash).
func (s *service) transitionRequest(ctx context.Context, op string, id uuid.UUID, in any,
	apply func(ctx context.Context, cur RefundRequest) (RefundRequest, error)) error {
	hash, err := idempotency.Hash(struct {
		Op      string
		Request uuid.UUID
		In      any
	}{op, id, in})
	if err != nil {
		return err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetRefundRequest(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		stored, err := apply(ctx, cur)
		return stored.ID, err
	})
	return err
}
