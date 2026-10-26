package ledger

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/parties"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// registerPayouts adds the balance and payout operations of BE-044 (04
// §2.17) to api.
func registerPayouts(api huma.API, h handler) {
	op.Register(api, "ledger", huma.Operation{
		OperationID: "getPayeeBalance", Method: http.MethodGet, Path: "/orgs/{org_id}/payees/{payee_id}/balance",
		Summary: "Get the earnings balance of a payee",
		Description: "What Koro owes the payee now (payable_amount: the payee's share of the paid orders minus what was " +
			"paid out; payouts in progress included), the part of it in payouts waiting for approval or for the transfer " +
			"(pending_payout_amount) and what was paid out so far (paid_out_amount). Amounts are kuruş. A payee of another " +
			"organization is 404.",
	}, op.Meta{Action: string(authz.LedgerBalanceRead), Surface: op.SurfaceOrg}, h.balance)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "listOrgPayouts", Method: http.MethodGet, Path: "/orgs/{org_id}/payouts",
		Summary:     "List the payouts of an organization",
		Description: "Newest first. The IBAN is masked.",
	}, op.Meta{Action: string(authz.LedgerPayoutList), Surface: op.SurfaceOrg}, h.listOrg)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "getOrgPayout", Method: http.MethodGet, Path: "/orgs/{org_id}/payouts/{payout_id}",
		Summary: "Get a payout of an organization",
		Description: "The payout with the order lines it pays and, once paid, a download URL of the bank receipt valid " +
			"for 5 minutes. The IBAN is masked. A payout of another organization is 404.",
	}, op.Meta{Action: string(authz.LedgerPayoutRead), Surface: op.SurfaceOrg}, h.getOrg)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "downloadOrgPayoutReceipt", Method: http.MethodGet, Path: "/orgs/{org_id}/payouts/{payout_id}/receipt",
		Summary: "Get a download URL of a payout's bank receipt",
		Description: "For the same people as getOrgPayout: a URL of the bank receipt (dekont) valid for 5 minutes. " +
			"Only a paid payout has one (409 ledger.invalid_state); 409 files.not_clean until the virus scan clears " +
			"the file. A payout of another organization is 404. Every download is audited.",
	}, op.Meta{Action: string(authz.LedgerPayoutRead), Surface: op.SurfaceOrg}, h.orgReceipt)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "adminListPayables", Method: http.MethodGet, Path: "/admin/payables",
		Summary: "List the order lines a payout may take",
		Description: "Per payee (the one asked for, or every payee the ledger owes money to): the paid, fulfilled lines " +
			"whose payout hold has elapsed as of as_of (default now), with a positive seller share after refunds, not paid out and in no payout yet.",
	}, op.Meta{Action: string(authz.LedgerPayableList), Surface: op.SurfaceAdmin}, h.payables)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "adminListPayouts", Method: http.MethodGet, Path: "/admin/payouts",
		Summary:     "List every payout",
		Description: "Newest first; status narrows the list (e.g. the approval queue: pending_approval).",
	}, op.Meta{Action: string(authz.LedgerPayoutList), Surface: op.SurfaceAdmin}, h.adminList)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "adminGetPayout", Method: http.MethodGet, Path: "/admin/payouts/{payout_id}",
		Summary: "Get a payout",
		Description: "The payout with its eyes, its items and, once paid, the receipt URL. The IBAN snapshot is masked " +
			"(BE-129): the finance admin who types the EFT reveals it with adminRevealPayoutIban.",
	}, op.Meta{Action: string(authz.LedgerPayoutRead), Surface: op.SurfaceAdmin}, h.adminGet)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "adminRevealPayoutIban", Method: http.MethodGet, Path: "/admin/payouts/{payout_id}/iban",
		Summary: "Reveal the full IBAN of a payout",
		Description: "The IBAN frozen on the payout at creation (iban_masked's full form), for the EFT at the bank. " +
			parties.RevealNote,
	}, op.Meta{Action: string(authz.LedgerIbanReveal), Surface: op.SurfaceAdmin, RateLimit: op.RateBreakGlass}, h.adminRevealIBAN)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "adminListPayeeReceivables", Method: http.MethodGet, Path: "/admin/payees/{payee_id}/receivables",
		Summary: "List the open receivable lines of a payee",
		Description: "BE-129, 06 H-59: the paid-out order lines whose refund claimed the seller share back and that the " +
			"payee still owes (amount: the open part, kuruş), in line order. A receivable collection " +
			"(adminCreateReceivableCollection) claims one of them by its line_id; a requested collection does not " +
			"close it until approved. getPayeeBalance's receivable_amount is their sum. An unknown payee is 404.",
	}, op.Meta{Action: string(authz.LedgerBalanceRead), Surface: op.SurfaceAdmin}, h.adminReceivables)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "adminCreatePayout", Method: http.MethodPost, Path: "/admin/payouts", DefaultStatus: http.StatusCreated,
		Summary: "Create a payout",
		Description: "Takes the payee's payable lines (all, or the order_line_ids given: each must be payable, 409 " +
			"ledger.line_not_payable; none: 422 ledger.nothing_payable) into a payout waiting for approval. The IBAN and " +
			"holder of the payee's approved bank account are frozen on it (none: 409 parties.no_approved_account). " +
			"Another finance admin approves it.",
	}, op.Meta{Action: string(authz.LedgerPayoutCreate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyRequired}, h.create)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "adminApprovePayout", Method: http.MethodPost, Path: "/admin/payouts/{payout_id}/approve",
		Summary: "Approve a payout (second eye)",
		Description: "Requires re-authentication. The finance admin who created the payout cannot approve it (403 " +
			"authz.four_eyes_violation). Only a payout waiting for approval (409 ledger.invalid_state); the frozen bank " +
			"account must still be the payee's approved account (409 ledger.bank_account_changed).",
	}, op.Meta{Action: string(authz.LedgerPayoutApprove), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.approve)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "adminMarkPayoutPaid", Method: http.MethodPost, Path: "/admin/payouts/{payout_id}/mark-paid",
		Summary: "Record the transfer of an approved payout",
		Description: "After the EFT at the bank: the bank reference and the receipt (a payout_receipt upload) are " +
			"required. In one transaction the ledger records the payout (payout_paid), the order lines are marked paid " +
			"out and payout.paid is written. Only an approved payout (409 ledger.invalid_state).",
	}, op.Meta{Action: string(authz.LedgerPayoutMarkPaid), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.markPaid)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "adminMarkPayoutFailed", Method: http.MethodPost, Path: "/admin/payouts/{payout_id}/mark-failed",
		Summary:     "Record that the transfer of an approved payout failed",
		Description: "Only an approved payout (409 ledger.invalid_state); its lines are payable again.",
	}, op.Meta{Action: string(authz.LedgerPayoutMarkFailed), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.markFailed)

	op.Register(api, "ledger", huma.Operation{
		OperationID: "adminCancelPayout", Method: http.MethodPost, Path: "/admin/payouts/{payout_id}/cancel",
		Summary:     "Cancel a payout before it is paid",
		Description: "Only a payout waiting for approval or approved (409 ledger.invalid_state); its lines are payable again.",
	}, op.Meta{Action: string(authz.LedgerPayoutCancel), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.cancel)
}

// sortPayouts is the only order of the payout lists (cursor fingerprint).
const sortPayouts = "-created_at"

func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---- DTOs

// PayoutItemDTO is one order line a payout pays.
type PayoutItemDTO struct {
	LineID  string `json:"line_id" format:"uuid"`
	OrderID string `json:"order_id" format:"uuid"`
	OrderNo string `json:"order_no" doc:"The order's bank transfer reference code"`
	Amount  int64  `json:"amount" doc:"Kuruş: the line's share of the payee"`
}

// PayoutDTO is a payout as the seller organization sees it (04 §2.17: the
// IBAN masked; the finance admins' identities are not shown).
type PayoutDTO struct {
	PayoutID            string          `json:"payout_id" format:"uuid"`
	PayeeID             string          `json:"payee_id" format:"uuid"`
	OrgID               string          `json:"org_id" format:"uuid"`
	Status              string          `json:"status" enum:"pending_approval,approved,paid,failed,cancelled"`
	Amount              int64           `json:"amount" doc:"Kuruş"`
	Currency            string          `json:"currency" enum:"TRY"`
	IBANMasked          string          `json:"iban_masked" doc:"The IBAN the payout goes to, masked"`
	HolderSnapshot      string          `json:"holder_snapshot" doc:"The account holder frozen at creation"`
	CreatedAt           time.Time       `json:"created_at"`
	ApprovedAt          *time.Time      `json:"approved_at,omitempty"`
	PaidAt              *time.Time      `json:"paid_at,omitempty"`
	BankReference       string          `json:"bank_reference,omitempty"`
	FailureReason       string          `json:"failure_reason,omitempty"`
	CancelledAt         *time.Time      `json:"cancelled_at,omitempty"`
	CancelReason        string          `json:"cancel_reason,omitempty"`
	Items               []PayoutItemDTO `json:"items,omitempty" doc:"The order lines paid (getOrgPayout; none once failed or cancelled)"`
	ReceiptURL          string          `json:"receipt_url,omitempty" deprecated:"true" doc:"Paid payouts: a download URL of the bank receipt. Deprecated (BE-127): use downloadOrgPayoutReceipt, which audits each download; this field will be removed"`
	ReceiptURLExpiresAt *time.Time      `json:"receipt_url_expires_at,omitempty" deprecated:"true"`
}

// PayoutAdminDTO is a payout as finance sees it (04 §C.5). The IBAN
// snapshot is masked like everywhere (BE-129: adminRevealPayoutIban).
type PayoutAdminDTO struct {
	PayoutDTO
	BankAccountID string `json:"bank_account_id" format:"uuid"`
	CreatedBy     string `json:"created_by" format:"uuid"`
	ApprovedBy    string `json:"approved_by,omitempty" format:"uuid"`
	PaidBy        string `json:"paid_by,omitempty" format:"uuid"`
	ReceiptFileID string `json:"receipt_file_id,omitempty" format:"uuid"`
}

func payoutDTO(v PayoutView) PayoutDTO {
	p := v.Payout
	d := PayoutDTO{
		PayoutID: p.ID.String(), PayeeID: p.PayeeID.String(), OrgID: p.OrgID.String(), Status: string(p.Status),
		Amount: p.Amount, Currency: p.Currency, IBANMasked: p.MaskedIBAN(), HolderSnapshot: p.HolderSnapshot,
		CreatedAt: p.CreatedAt.UTC(), ApprovedAt: db.UTCPtr(p.ApprovedAt), PaidAt: db.UTCPtr(p.PaidAt),
		BankReference: strVal(p.BankReference), FailureReason: strVal(p.FailureReason), CancelledAt: db.UTCPtr(p.CancelledAt),
		CancelReason: strVal(p.CancelReason), ReceiptURL: v.ReceiptURL, ReceiptURLExpiresAt: db.UTCPtr(v.ReceiptExp),
	}
	if d.Currency == "" {
		d.Currency = Currency
	}
	for _, it := range v.Items {
		d.Items = append(d.Items, PayoutItemDTO{LineID: it.OrderLineID.String(), OrderID: it.OrderID.String(),
			OrderNo: it.OrderNo, Amount: it.Amount})
	}
	return d
}

func payoutAdminDTO(v PayoutView) PayoutAdminDTO {
	p := v.Payout
	return PayoutAdminDTO{
		PayoutDTO: payoutDTO(v), BankAccountID: p.BankAccountID.String(),
		CreatedBy: p.CreatedBy.String(), ApprovedBy: httpx.IDStr(p.ApprovedBy), PaidBy: httpx.IDStr(p.PaidBy),
		ReceiptFileID: httpx.IDStr(p.ReceiptFileID),
	}
}

// ---- getPayeeBalance

type balanceInput struct {
	OrgID   string `path:"org_id" format:"uuid"`
	PayeeID string `path:"payee_id" format:"uuid"`
}

// PayeeBalanceDTO is the body of getPayeeBalance (04 §2.17).
type PayeeBalanceDTO struct {
	ReceivableAmount    int64  `json:"receivable_amount" doc:"Kuruş the payee owes Koro; never offset automatically"`
	PayeeID             string `json:"payee_id" format:"uuid"`
	PayableAmount       int64  `json:"payable_amount" doc:"Kuruş Koro owes the payee now (payouts in progress included)"`
	PendingPayoutAmount int64  `json:"pending_payout_amount" doc:"Kuruş in payouts waiting for approval or the transfer"`
	PaidOutAmount       int64  `json:"paid_out_amount" doc:"Kuruş paid out so far"`
	Currency            string `json:"currency" enum:"TRY"`
}

type balanceOutput struct{ Body PayeeBalanceDTO }

func (h handler) balance(ctx context.Context, in *balanceInput) (*balanceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b, err := h.svc.GetPayeeBalance(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.PayeeID))
	if err != nil {
		return nil, err
	}
	return &balanceOutput{Body: PayeeBalanceDTO{PayeeID: b.PayeeID.String(), PayableAmount: b.Payable, ReceivableAmount: b.Receivable,
		PendingPayoutAmount: b.PendingPayout, PaidOutAmount: b.PaidOut, Currency: Currency}}, nil
}

// ---- listOrgPayouts / adminListPayouts

type listOrgPayoutsInput struct {
	page.Params
	OrgID string `path:"org_id" format:"uuid"`
}

// PayoutList is the body of listOrgPayouts.
type PayoutList struct {
	Items      []PayoutDTO `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

type payoutListOutput struct{ Body PayoutList }

func (h handler) listOrg(ctx context.Context, in *listOrgPayoutsInput) (*payoutListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listOrgPayouts", in.OrgID)
	q := PayoutQuery{Limit: in.Limit}
	q.After, q.invalid = payoutAfter(in.Cursor, fp)
	p, err := h.svc.ListOrgPayouts(ctx, a, httpx.ParseID(in.OrgID), q)
	if err != nil {
		return nil, err
	}
	out := &payoutListOutput{Body: PayoutList{Items: make([]PayoutDTO, len(p.Items))}}
	for i, po := range p.Items {
		out.Body.Items[i] = payoutDTO(PayoutView{Payout: po})
	}
	out.Body.NextCursor = payoutNext(p.Next, fp)
	return out, nil
}

type adminListPayoutsInput struct {
	page.Params
	Status string `query:"status" enum:"pending_approval,approved,paid,failed,cancelled"`
}

// PayoutAdminList is the body of adminListPayouts.
type PayoutAdminList struct {
	Items      []PayoutAdminDTO `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type payoutAdminListOutput struct{ Body PayoutAdminList }

func (h handler) adminList(ctx context.Context, in *adminListPayoutsInput) (*payoutAdminListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListPayouts", in.Status)
	q := PayoutQuery{Status: PayoutStatus(in.Status), Limit: in.Limit}
	q.After, q.invalid = payoutAfter(in.Cursor, fp)
	p, err := h.svc.AdminListPayouts(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &payoutAdminListOutput{Body: PayoutAdminList{Items: make([]PayoutAdminDTO, len(p.Items))}}
	for i, po := range p.Items {
		out.Body.Items[i] = payoutAdminDTO(PayoutView{Payout: po})
	}
	out.Body.NextCursor = payoutNext(p.Next, fp)
	return out, nil
}

// payoutAfter decodes a payout cursor (created_at, id).
func payoutAfter(cursor, fp string) (*PayoutCursor, error) {
	c, err := page.Decode(cursor, sortPayouts, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	at, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	id, err := c.UUID(1)
	if err != nil {
		return nil, err
	}
	return &PayoutCursor{CreatedAt: at, ID: id}, nil
}

func payoutNext(next *PayoutCursor, fp string) string {
	if next == nil {
		return ""
	}
	return page.Next(sortPayouts, fp, page.TimeVal(next.CreatedAt), page.UUIDVal(next.ID))
}

// ---- getOrgPayout / adminGetPayout

type orgPayoutInput struct {
	OrgID    string `path:"org_id" format:"uuid"`
	PayoutID string `path:"payout_id" format:"uuid"`
}

type payoutOutput struct{ Body PayoutDTO }

func (h handler) getOrg(ctx context.Context, in *orgPayoutInput) (*payoutOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetOrgPayout(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.PayoutID))
	if err != nil {
		return nil, err
	}
	return &payoutOutput{Body: payoutDTO(v)}, nil
}

// PayoutReceiptDTO is the body of downloadOrgPayoutReceipt (04 §1.14
// signed URL).
type PayoutReceiptDTO struct {
	URL       string    `json:"url" doc:"Valid for at most 5 minutes"`
	ExpiresAt time.Time `json:"expires_at"`
}

type payoutReceiptOutput struct{ Body PayoutReceiptDTO }

func (h handler) orgReceipt(ctx context.Context, in *orgPayoutInput) (*payoutReceiptOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.OrgPayoutReceipt(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.PayoutID))
	if err != nil {
		return nil, err
	}
	return &payoutReceiptOutput{Body: PayoutReceiptDTO{URL: d.URL, ExpiresAt: d.ExpiresAt.UTC()}}, nil
}

// PayoutPath is the path of the /admin/payouts/{payout_id} operations.
type PayoutPath struct {
	PayoutID string `path:"payout_id" format:"uuid"`
}

type payoutAdminOutput struct{ Body PayoutAdminDTO }

func adminOut(v PayoutView) *payoutAdminOutput { return &payoutAdminOutput{Body: payoutAdminDTO(v)} }

type adminGetInput struct{ PayoutPath }

func (h handler) adminGet(ctx context.Context, in *adminGetInput) (*payoutAdminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminGetPayout(ctx, a, httpx.ParseID(in.PayoutID))
	if err != nil {
		return nil, err
	}
	return adminOut(v), nil
}

type adminRevealInput struct {
	PayoutPath
	httpx.AccessReasonHeader
}

func (h handler) adminRevealIBAN(ctx context.Context, in *adminRevealInput) (*parties.RevealOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	n, err := h.svc.AdminRevealPayoutIBAN(ctx, a, httpx.ParseID(in.PayoutID), in.AccessReason.String())
	if err != nil {
		return nil, err
	}
	return parties.RevealOut(n), nil
}

// ---- adminListPayeeReceivables

// ReceivableLineDTO is one open receivable line of a payee.
type ReceivableLineDTO struct {
	LineID   string `json:"line_id" format:"uuid" doc:"The order_line_id of adminCreateReceivableCollection"`
	OrderID  string `json:"order_id" format:"uuid"`
	OrderNo  string `json:"order_no" doc:"The order's bank transfer reference code"`
	PayoutID string `json:"payout_id" format:"uuid" doc:"The payout that paid the line out"`
	Amount   int64  `json:"amount" doc:"Kuruş the payee still owes for the line"`
	Currency string `json:"currency" enum:"TRY"`
}

// ReceivableLineList is the body of adminListPayeeReceivables.
type ReceivableLineList struct {
	Items       []ReceivableLineDTO `json:"items"`
	TotalAmount int64               `json:"total_amount" doc:"Kuruş: getPayeeBalance's receivable_amount"`
	Currency    string              `json:"currency" enum:"TRY"`
}

type receivablesInput struct {
	PayeeID string `path:"payee_id" format:"uuid"`
}

type receivablesOutput struct{ Body ReceivableLineList }

func (h handler) adminReceivables(ctx context.Context, in *receivablesInput) (*receivablesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ls, err := h.svc.AdminListPayeeReceivables(ctx, a, httpx.ParseID(in.PayeeID))
	if err != nil {
		return nil, err
	}
	out := &receivablesOutput{Body: ReceivableLineList{Items: make([]ReceivableLineDTO, len(ls)), Currency: Currency}}
	for i, l := range ls {
		out.Body.Items[i] = ReceivableLineDTO{LineID: l.OrderLineID.String(), OrderID: l.OrderID.String(), OrderNo: l.OrderNo,
			PayoutID: l.PayoutID.String(), Amount: l.Amount, Currency: Currency}
		out.Body.TotalAmount += l.Amount
	}
	return out, nil
}

// ---- adminListPayables

type payablesInput struct {
	PayeeID string `query:"payee_id" format:"uuid" doc:"Only this payee"`
	AsOf    string `query:"as_of" maxLength:"40" doc:"RFC 3339 time or YYYY-MM-DD (end of that day, Europe/Istanbul); default now"`
}

// PayableLineDTO is one payable order line.
type PayableLineDTO struct {
	LineID      string    `json:"line_id" format:"uuid"`
	OrderID     string    `json:"order_id" format:"uuid"`
	OrderNo     string    `json:"order_no"`
	Amount      int64     `json:"amount" doc:"Kuruş: the payee's share of the line (seller net)"`
	FulfilledAt time.Time `json:"fulfilled_at"`
}

// PayableGroupDTO is the payable lines of one payee.
type PayableGroupDTO struct {
	PayeeID     string           `json:"payee_id" format:"uuid"`
	OrgID       string           `json:"org_id" format:"uuid" doc:"The seller organization"`
	Lines       []PayableLineDTO `json:"lines"`
	TotalAmount int64            `json:"total_amount" doc:"Kuruş"`
	Currency    string           `json:"currency" enum:"TRY"`
}

// PayableList is the body of adminListPayables.
type PayableList struct {
	Items []PayableGroupDTO `json:"items"`
}

type payablesOutput struct{ Body PayableList }

func (h handler) payables(ctx context.Context, in *payablesInput) (*payablesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var q PayablesQuery
	if in.PayeeID != "" {
		id, err := uuid.Parse(in.PayeeID)
		if err != nil {
			q.invalid = problem.Field("query.payee_id", "must be a UUID")
		} else {
			q.PayeeID = &id
		}
	}
	if in.AsOf != "" && q.invalid == nil {
		t, err := parseBound(in.AsOf, true)
		if err != nil {
			q.invalid = problem.Field("query.as_of", "must be an RFC 3339 time or a YYYY-MM-DD date")
		} else {
			q.AsOf = &t
		}
	}
	gs, err := h.svc.AdminListPayables(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &payablesOutput{Body: PayableList{Items: make([]PayableGroupDTO, len(gs))}}
	for i, g := range gs {
		d := PayableGroupDTO{PayeeID: g.PayeeID.String(), OrgID: g.OrgID.String(), TotalAmount: g.Total, Currency: Currency,
			Lines: make([]PayableLineDTO, len(g.Lines))}
		for j, l := range g.Lines {
			d.Lines[j] = PayableLineDTO{LineID: l.OrderLineID.String(), OrderID: l.OrderID.String(), OrderNo: l.OrderNo,
				Amount: l.Amount, FulfilledAt: l.FulfilledAt.UTC()}
		}
		out.Body.Items[i] = d
	}
	return out, nil
}

// ---- adminCreatePayout

type createInput struct {
	idempotency.RequiredHeader
	Body struct {
		PayeeID      string   `json:"payee_id" format:"uuid" doc:"The payee to pay"`
		OrderLineIDs []string `json:"order_line_ids,omitempty" maxItems:"500" doc:"Only these payable lines (default: every payable line of the payee)"`
	}
}

func (h handler) create(ctx context.Context, in *createInput) (*payoutAdminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ci := CreatePayoutInput{PayeeID: httpx.ParseID(in.Body.PayeeID)}
	for _, s := range in.Body.OrderLineIDs {
		ci.LineIDs = append(ci.LineIDs, httpx.ParseID(s))
	}
	v, err := h.svc.AdminCreatePayout(ctx, a, ci)
	if err != nil {
		return nil, err
	}
	return adminOut(v), nil
}

// ---- adminApprovePayout

type approveInput struct {
	idempotency.OptionalHeader
	PayoutPath
	Body struct {
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) approve(ctx context.Context, in *approveInput) (*payoutAdminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var proof identity.ReauthProof
	if r := in.Body.Reauth; r != nil {
		proof = identity.ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
	}
	v, err := h.svc.AdminApprovePayout(ctx, a, httpx.ParseID(in.PayoutID), proof)
	if err != nil {
		return nil, err
	}
	return adminOut(v), nil
}

// ---- adminMarkPayoutPaid

type markPaidInput struct {
	idempotency.OptionalHeader
	PayoutPath
	Body struct {
		BankReference string     `json:"bank_reference" minLength:"1" maxLength:"100" doc:"The bank's reference of the transfer"`
		ReceiptFileID string     `json:"receipt_file_id" format:"uuid" doc:"The bank receipt (dekont): a payout_receipt upload"`
		PaidAt        *time.Time `json:"paid_at,omitempty" doc:"When the transfer left Koro's bank (default now)"`
	}
}

func (h handler) markPaid(ctx context.Context, in *markPaidInput) (*payoutAdminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	mi := MarkPaidInput{BankReference: in.Body.BankReference, PaidAt: in.Body.PaidAt}
	if in.Body.ReceiptFileID != "" {
		id := httpx.ParseID(in.Body.ReceiptFileID)
		mi.ReceiptFileID = &id
	}
	v, err := h.svc.AdminMarkPayoutPaid(ctx, a, httpx.ParseID(in.PayoutID), mi)
	if err != nil {
		return nil, err
	}
	return adminOut(v), nil
}

// ---- adminMarkPayoutFailed / adminCancelPayout

type markFailedInput struct {
	idempotency.OptionalHeader
	PayoutPath
	Body struct {
		FailureReason string `json:"failure_reason" minLength:"1" maxLength:"500" doc:"Why the transfer failed (no personal data)"`
	}
}

func (h handler) markFailed(ctx context.Context, in *markFailedInput) (*payoutAdminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminMarkPayoutFailed(ctx, a, httpx.ParseID(in.PayoutID), in.Body.FailureReason)
	if err != nil {
		return nil, err
	}
	return adminOut(v), nil
}

type cancelInput struct {
	idempotency.OptionalHeader
	PayoutPath
	Body struct {
		CancelReason string `json:"cancel_reason" minLength:"1" maxLength:"500" doc:"Why the payout is cancelled (no personal data)"`
	}
}

func (h handler) cancel(ctx context.Context, in *cancelInput) (*payoutAdminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminCancelPayout(ctx, a, httpx.ParseID(in.PayoutID), in.Body.CancelReason)
	if err != nil {
		return nil, err
	}
	return adminOut(v), nil
}
