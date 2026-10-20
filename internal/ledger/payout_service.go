package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/money"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// PayoutService is the payee balance and the payout flow of 04 §2.17
// (BE-044; 03 §5.17, §6.1 "Payout"). Every method calls authz.Can first
// (05 §5.3). Money moves only on mark-paid, in one transaction with the
// ledger, the order lines and the outbox (01 §6.3 "Payout ödendi").
type PayoutService interface {
	// GetPayeeBalance is getPayeeBalance (ledger.balance.read): the payee
	// must belong to the org of the path (06 H-38; another org's payee 404).
	GetPayeeBalance(ctx context.Context, a authz.Actor, orgID, payeeID uuid.UUID) (Balance, error)
	// ListOrgPayouts is listOrgPayouts (ledger.payout.list): the seller org's
	// payouts, newest first.
	ListOrgPayouts(ctx context.Context, a authz.Actor, orgID uuid.UUID, q PayoutQuery) (PayoutPage, error)
	// GetOrgPayout is getOrgPayout (ledger.payout.read): a payout of the org
	// with its items and, once paid, a download URL of its receipt.
	GetOrgPayout(ctx context.Context, a authz.Actor, orgID, payoutID uuid.UUID) (PayoutView, error)
	// AdminListPayables is adminListPayables (ledger.payable.list).
	AdminListPayables(ctx context.Context, a authz.Actor, q PayablesQuery) ([]PayableGroup, error)
	// AdminListPayouts is adminListPayouts (ledger.payout.list).
	AdminListPayouts(ctx context.Context, a authz.Actor, q PayoutQuery) (PayoutPage, error)
	// AdminGetPayout is adminGetPayout (ledger.payout.read).
	AdminGetPayout(ctx context.Context, a authz.Actor, payoutID uuid.UUID) (PayoutView, error)
	// AdminCreatePayout is adminCreatePayout (ledger.payout.create;
	// Idempotency-Key): the payable lines are taken into a pending payout
	// and the approved IBAN is frozen on it.
	AdminCreatePayout(ctx context.Context, a authz.Actor, in CreatePayoutInput) (PayoutView, error)
	// AdminApprovePayout is adminApprovePayout (ledger.payout.approve: four
	// eyes, re-authentication).
	AdminApprovePayout(ctx context.Context, a authz.Actor, payoutID uuid.UUID, proof identity.ReauthProof) (PayoutView, error)
	// AdminMarkPayoutPaid is adminMarkPayoutPaid (ledger.payout.mark_paid):
	// receipt and bank reference required.
	AdminMarkPayoutPaid(ctx context.Context, a authz.Actor, payoutID uuid.UUID, in MarkPaidInput) (PayoutView, error)
	// AdminMarkPayoutFailed is adminMarkPayoutFailed
	// (ledger.payout.mark_failed): the lines are payable again.
	AdminMarkPayoutFailed(ctx context.Context, a authz.Actor, payoutID uuid.UUID, reason string) (PayoutView, error)
	// AdminCancelPayout is adminCancelPayout (ledger.payout.cancel): only
	// before it is paid; the lines are payable again.
	AdminCancelPayout(ctx context.Context, a authz.Actor, payoutID uuid.UUID, reason string) (PayoutView, error)
}

// Limits of the payout operations.
const (
	maxPayoutLines   = 500 // order_line_ids of one adminCreatePayout
	maxPayableGroups = 200 // payees of one adminListPayables without payee_id
	maxReasonRunes   = 500 // failure_reason, cancel_reason (000113)
	maxBankRefRunes  = 100 // payouts_bank_reference_chk
	receiptURLTTL    = 5 * time.Minute
	// paidAtSkew tolerates a client clock slightly ahead of ours.
	paidAtSkew = 5 * time.Minute
	// payoutAggregate is the aggregate type of the payout events.
	payoutAggregate = "payout"
)

// payoutsOn reports 503 when the payout flow is not wired (Deps.Payouts).
func (s *service) payoutsOn() error {
	if s.d.Payouts == nil {
		return problem.ErrUnavailable
	}
	return nil
}

// now is the service clock in the database's precision.
func (s *service) now() time.Time { return payoutStamp(s.d.Clock.Now()) }

// orgPayees is the authz view of an org's payees for the balance (03 §3.5:
// OrgID = the org of the path; authorised before any payee is read).
func orgPayees(orgID uuid.UUID) authz.Resource {
	org := orgID
	return authz.Resource{Type: authz.TypePayee, OrgID: &org}
}

// orgPayouts is the authz view of an org's payouts as a collection (the org
// surface: authorised before any payout is read; no ID probing).
func orgPayouts(orgID uuid.UUID) authz.Resource {
	org := orgID
	return authz.Resource{Type: authz.TypePayout, OrgID: &org,
		Attrs: map[string]string{authz.AttrStatus: "", authz.AttrCreatedBy: ""}}
}

// payoutResource is the authz view of one payout (03 §3.5: Attrs status and
// created_by, the four eyes of the approval).
func payoutResource(p Payout) authz.Resource {
	org := p.OrgID
	return authz.Resource{Type: authz.TypePayout, ID: p.ID, OrgID: &org,
		Attrs: map[string]string{authz.AttrStatus: string(p.Status), authz.AttrCreatedBy: p.CreatedBy.String()}}
}

// payoutEvent is the audit record of a payout write: IDs, states and
// amounts; the IBAN masked only.
func payoutEvent(a authz.Actor, act authz.Action, p Payout, reason string, meta map[string]any) audit.Event {
	id, org := p.ID, p.OrgID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["payee_id"] = p.PayeeID.String()
	meta["status"] = string(p.Status)
	meta["amount"] = p.Amount
	meta["iban_masked"] = p.MaskedIBAN()
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: authz.TypePayout, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// emit writes a payout event: O topics into the outbox in the transaction,
// A topics after the commit (01 §6.2).
func (s *service) emit(ctx context.Context, p Payout, payload domainevent.Payload) error {
	env, err := domainevent.New(payload, payoutAggregate, p.ID)
	if err != nil {
		return err
	}
	if payload.Topic() == domainevent.TopicPayoutCreated {
		return s.d.Outbox.AfterCommit(ctx, env)
	}
	return s.d.Outbox.Add(ctx, env)
}

// orgSurface resolves the org of an org-surface request (unknown → 404)
// and authorises act on r before anything else is read.
func (s *service) orgSurface(ctx context.Context, a authz.Actor, act authz.Action, orgID uuid.UUID, r authz.Resource) error {
	if err := s.payoutsOn(); err != nil {
		return err
	}
	if _, err := s.d.Orgs.Get(ctx, orgID); err != nil {
		return err
	}
	return authz.Can(ctx, a, act, r)
}

// adminGate authorises an /admin operation before anything is read (03
// §7.2-3: an actor without the role gets 403).
func (s *service) adminGate(ctx context.Context, a authz.Actor, act authz.Action) error {
	if err := s.payoutsOn(); err != nil {
		return err
	}
	return authz.Can(ctx, a, act, adminSurface)
}

// ---- org surface

// GetPayeeBalance implements PayoutService.
func (s *service) GetPayeeBalance(ctx context.Context, a authz.Actor, orgID, payeeID uuid.UUID) (Balance, error) {
	if err := s.orgSurface(ctx, a, authz.LedgerBalanceRead, orgID, orgPayees(orgID)); err != nil {
		return Balance{}, err
	}
	p, err := s.d.Payees.Get(ctx, payeeID)
	switch {
	case errors.Is(err, parties.ErrNotFound):
		return Balance{}, ErrNotFound
	case err != nil:
		return Balance{}, err
	case p.OrgID != orgID:
		return Balance{}, ErrNotFound // another org's payee (06 H-38)
	}
	bal, err := s.d.Store.PayeeBalance(ctx, p.ID)
	if err != nil {
		return Balance{}, err
	}
	amts, err := s.d.Payouts.PayoutAmounts(ctx, p.ID)
	if err != nil {
		return Balance{}, err
	}
	receivable, err := s.d.Store.PayeeReceivable(ctx, p.ID)
	if err != nil {
		return Balance{}, err
	}
	return Balance{PayeeID: p.ID, Payable: bal, PendingPayout: amts.Pending, PaidOut: amts.PaidOut, Receivable: receivable}, nil
}

// ListOrgPayouts implements PayoutService.
func (s *service) ListOrgPayouts(ctx context.Context, a authz.Actor, orgID uuid.UUID, q PayoutQuery) (PayoutPage, error) {
	if err := s.orgSurface(ctx, a, authz.LedgerPayoutList, orgID, orgPayouts(orgID)); err != nil {
		return PayoutPage{}, err
	}
	org := orgID
	return s.listPayouts(ctx, PayoutFilter{OrgID: &org}, q)
}

// GetOrgPayout implements PayoutService.
func (s *service) GetOrgPayout(ctx context.Context, a authz.Actor, orgID, payoutID uuid.UUID) (PayoutView, error) {
	if err := s.orgSurface(ctx, a, authz.LedgerPayoutRead, orgID, orgPayouts(orgID)); err != nil {
		return PayoutView{}, err
	}
	p, err := s.d.Payouts.GetPayout(ctx, payoutID, false)
	if err != nil {
		return PayoutView{}, err
	}
	if p.OrgID != orgID {
		return PayoutView{}, ErrNotFound // another org's payout (06 H-38)
	}
	return s.view(ctx, p)
}

// ---- admin surface

// AdminListPayables implements PayoutService: per payee (the one asked
// for, or every payee the ledger owes money to) the lines a payout could
// take as of as_of, without the lines already in a live payout.
func (s *service) AdminListPayables(ctx context.Context, a authz.Actor, q PayablesQuery) ([]PayableGroup, error) {
	if err := s.adminGate(ctx, a, authz.LedgerPayableList); err != nil {
		return nil, err
	}
	if q.invalid != nil {
		return nil, q.invalid
	}
	asOf := s.now()
	if q.AsOf != nil {
		asOf = q.AsOf.UTC()
	}
	var payees []uuid.UUID
	if q.PayeeID != nil {
		payees = []uuid.UUID{*q.PayeeID}
	} else {
		var err error
		if payees, err = s.d.Payouts.PayeesOwed(ctx, maxPayableGroups); err != nil {
			return nil, err
		}
	}
	out := []PayableGroup{}
	for _, payee := range payees {
		lines, err := s.payableLines(ctx, payee, asOf)
		if err != nil {
			return nil, err
		}
		if len(lines) == 0 {
			continue
		}
		g := PayableGroup{PayeeID: payee, OrgID: lines[0].SellerOrgID, Lines: make([]PayableLineView, len(lines))}
		amounts := make([]int64, len(lines))
		for i, l := range lines {
			g.Lines[i] = PayableLineView{OrderLineID: l.LineID, OrderID: l.OrderID, OrderNo: l.OrderNo, Amount: l.SellerNetAmount,
				FulfilledAt: l.FulfilledAt.UTC()}
			amounts[i] = l.SellerNetAmount
		}
		if g.Total, err = money.Sum(amounts...); err != nil {
			return nil, fmt.Errorf("ledger: payable total of %s: %w", payee, err)
		}
		out = append(out, g)
	}
	return out, nil
}

// payableLines returns the payee's unheld lines with a positive seller
// remainder after completed refunds. Frozen order snapshots stay unchanged.
func (s *service) payableLines(ctx context.Context, payeeID uuid.UUID, asOf time.Time) ([]orders.PayableLine, error) {
	lines, err := s.d.Orders.PayableLines(ctx, payeeID, asOf)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(lines))
	for i, l := range lines {
		ids[i] = l.LineID
	}
	held, err := s.d.Payouts.LinesInPayouts(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := lines[:0]
	for _, l := range lines {
		if _, in := held[l.LineID]; in || l.SellerNetAmount <= 0 || l.PayeeID != payeeID {
			continue
		}
		refunded, err := s.d.Store.RefundedShares(ctx, l.LineID)
		if err != nil {
			return nil, err
		}
		if refunded.Seller < 0 || refunded.Seller > l.SellerNetAmount {
			return nil, ErrInvalidState
		}
		l.SellerNetAmount -= refunded.Seller
		if l.SellerNetAmount > 0 {
			out = append(out, l)
		}
	}
	return out, nil
}

// AdminListPayouts implements PayoutService.
func (s *service) AdminListPayouts(ctx context.Context, a authz.Actor, q PayoutQuery) (PayoutPage, error) {
	if err := s.adminGate(ctx, a, authz.LedgerPayoutList); err != nil {
		return PayoutPage{}, err
	}
	if q.Status != "" && !q.Status.Valid() {
		return PayoutPage{}, fieldErr("query.status", "is not a payout status")
	}
	return s.listPayouts(ctx, PayoutFilter{Status: q.Status}, q)
}

func (s *service) listPayouts(ctx context.Context, f PayoutFilter, q PayoutQuery) (PayoutPage, error) {
	if q.invalid != nil {
		return PayoutPage{}, q.invalid
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Payouts.ListPayouts(ctx, f, q.After, limit+1)
	if err != nil {
		return PayoutPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := PayoutPage{Items: rows}
	if more {
		last := rows[len(rows)-1]
		out.Next = &PayoutCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// AdminGetPayout implements PayoutService.
func (s *service) AdminGetPayout(ctx context.Context, a authz.Actor, payoutID uuid.UUID) (PayoutView, error) {
	if err := s.adminGate(ctx, a, authz.LedgerPayoutRead); err != nil {
		return PayoutView{}, err
	}
	p, err := s.d.Payouts.GetPayout(ctx, payoutID, false)
	if err != nil {
		return PayoutView{}, err
	}
	return s.view(ctx, p)
}

// view adds the items (with their order references) and, for a paid
// payout, a short-lived URL of its receipt. A failed or cancelled payout
// has no items any more (its lines were released).
func (s *service) view(ctx context.Context, p Payout) (PayoutView, error) {
	items, err := s.d.Payouts.PayoutItems(ctx, p.ID)
	if err != nil {
		return PayoutView{}, err
	}
	v := PayoutView{Payout: p, Items: make([]PayoutItemView, len(items))}
	orderNo := map[uuid.UUID]string{}
	for i, it := range items {
		l, err := s.d.Orders.GetLine(ctx, it.OrderLineID)
		if err != nil {
			return PayoutView{}, fmt.Errorf("ledger: payout %s line %s: %w", p.ID, it.OrderLineID, err)
		}
		no, ok := orderNo[l.OrderID]
		if !ok {
			o, err := s.d.Orders.ForPayment(ctx, l.OrderID)
			if err != nil {
				return PayoutView{}, fmt.Errorf("ledger: payout %s order %s: %w", p.ID, l.OrderID, err)
			}
			no, orderNo[l.OrderID] = o.OrderNo, o.OrderNo
		}
		v.Items[i] = PayoutItemView{OrderLineID: it.OrderLineID, OrderID: l.OrderID, OrderNo: no, Amount: it.Amount}
	}
	if p.Status == StatusPaid && p.ReceiptFileID != nil {
		d, err := s.d.Files.PresignGet(ctx, *p.ReceiptFileID, receiptURLTTL)
		switch {
		case errors.Is(err, files.ErrNotClean):
			// Not scanned clean (yet): the payout is shown without the link.
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "payout receipt not served: file not clean",
				slog.String("payout_id", p.ID.String()))
		case err != nil:
			return PayoutView{}, err
		default:
			exp := d.ExpiresAt.UTC()
			v.ReceiptURL, v.ReceiptExp = d.URL, &exp
		}
	}
	return v, nil
}

// AdminCreatePayout implements PayoutService: authz (403) → the input (422)
// → in one transaction with the Idempotency-Key: the payee (422 unknown,
// 409 not payable now) and its approved account (409
// parties.no_approved_account) → the payee is locked → its payable lines
// without those in a live payout (requested ones must all be there: 409
// ledger.line_not_payable; none: 422 ledger.nothing_payable) → the ledger
// must owe at least the open payouts plus this one (409
// ledger.negative_payee_balance) → the pending payout with the IBAN and the
// holder of the approved account frozen on it, and its items → audit →
// payout.created (A: the other finance admins are asked to approve).
func (s *service) AdminCreatePayout(ctx context.Context, a authz.Actor, in CreatePayoutInput) (PayoutView, error) {
	if err := s.adminGate(ctx, a, authz.LedgerPayoutCreate); err != nil {
		return PayoutView{}, err
	}
	lines := slices.Clone(in.LineIDs)
	slices.SortFunc(lines, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
	switch {
	case in.PayeeID == uuid.Nil():
		return PayoutView{}, fieldErr("body.payee_id", "is required")
	case len(lines) > maxPayoutLines:
		return PayoutView{}, fieldErr("body.order_line_ids", fmt.Sprintf("must be at most %d", maxPayoutLines))
	case len(slices.Compact(slices.Clone(lines))) != len(lines):
		return PayoutView{}, fieldErr("body.order_line_ids", "lists a line twice")
	case slices.Contains(lines, uuid.Nil()):
		return PayoutView{}, fieldErr("body.order_line_ids", "must be UUIDs")
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		Payee uuid.UUID
		Lines []uuid.UUID
	}{"create_payout", in.PayeeID, lines})
	if err != nil {
		return PayoutView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		return s.createPayout(ctx, a, in.PayeeID, lines)
	})
	if err != nil {
		return PayoutView{}, translatePayout(err)
	}
	return s.reload(ctx, id)
}

func (s *service) createPayout(ctx context.Context, a authz.Actor, payeeID uuid.UUID, want []uuid.UUID) (uuid.UUID, error) {
	py, err := s.d.Payees.Get(ctx, payeeID)
	switch {
	case errors.Is(err, parties.ErrNotFound):
		return uuid.Nil(), fieldErr("body.payee_id", "is not a payee")
	case err != nil:
		return uuid.Nil(), err
	}
	active, err := s.d.Payees.IsActive(ctx, py.ID)
	if err != nil {
		return uuid.Nil(), err
	}
	if !active {
		return uuid.Nil(), ErrInvalidState.WithFields(problem.FieldError{Location: "body.payee_id", Message: "the payee may not be paid now"})
	}
	acct, err := s.d.Payees.ApprovedAccount(ctx, py.ID)
	if err != nil {
		return uuid.Nil(), err // 409 parties.no_approved_account
	}
	if acct.PayeeID != py.ID {
		return uuid.Nil(), fmt.Errorf("ledger: approved account %s is not payee %s's", acct.ID, py.ID)
	}
	// One creation per payee at a time: the lines read here are the lines
	// the items take (a concurrent create waits, then sees them held).
	if err := s.d.Store.LockPayees(ctx, []uuid.UUID{py.ID}); err != nil {
		return uuid.Nil(), err
	}
	now := s.now()
	payable, err := s.payableLines(ctx, py.ID, now)
	if err != nil {
		return uuid.Nil(), err
	}
	chosen := payable
	if len(want) > 0 {
		byID := make(map[uuid.UUID]orders.PayableLine, len(payable))
		for _, l := range payable {
			byID[l.LineID] = l
		}
		chosen = make([]orders.PayableLine, 0, len(want))
		for i, id := range want {
			l, ok := byID[id]
			if !ok {
				return uuid.Nil(), ErrLineNotPayable.WithFields(problem.FieldError{
					Location: fmt.Sprintf("body.order_line_ids[%d]", i), Message: "is not payable to the payee now"})
			}
			chosen = append(chosen, l)
		}
	}
	if len(chosen) == 0 {
		return uuid.Nil(), ErrNothingPayable
	}
	p := Payout{
		ID: uuid.NewV7(), PayeeID: py.ID, OrgID: py.OrgID, BankAccountID: acct.ID, IBANSnapshot: acct.IBAN,
		HolderSnapshot: acct.HolderName, Currency: Currency, Status: StatusPendingApproval, CreatedBy: a.UserID,
	}
	items := make([]PayoutItem, len(chosen))
	amounts := make([]int64, len(chosen))
	lineIDs := make([]any, len(chosen))
	for i, l := range chosen {
		items[i] = PayoutItem{PayoutID: p.ID, OrderLineID: l.LineID, PayeeID: py.ID, Amount: l.SellerNetAmount, CreatedAt: now}
		amounts[i], lineIDs[i] = l.SellerNetAmount, l.LineID.String()
	}
	if p.Amount, err = money.Sum(amounts...); err != nil {
		return uuid.Nil(), invalid("body.order_line_ids", "the amounts overflow")
	}
	// The ledger must owe what the open payouts and this one will pay out
	// (a line whose allocation was never posted cannot be paid out).
	bal, err := s.d.Store.PayeeBalance(ctx, py.ID)
	if err != nil {
		return uuid.Nil(), err
	}
	amts, err := s.d.Payouts.PayoutAmounts(ctx, py.ID)
	if err != nil {
		return uuid.Nil(), err
	}
	if need, err := money.Sum(amts.Pending, p.Amount); err != nil || bal < need {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "payout refused: the ledger owes the payee less than the payouts",
			slog.String("payee_id", py.ID.String()), slog.Int64("balance", bal), slog.Int64("pending", amts.Pending),
			slog.Int64("amount", p.Amount))
		return uuid.Nil(), ErrNegativeBalance
	}
	stored, err := s.d.Payouts.InsertPayout(ctx, p, items)
	if err != nil {
		return uuid.Nil(), err
	}
	if err := s.d.Audit.Log(ctx, payoutEvent(a, authz.LedgerPayoutCreate, stored, "", map[string]any{
		"bank_account_id": stored.BankAccountID.String(), "order_line_ids": lineIDs,
	})); err != nil {
		return uuid.Nil(), err
	}
	if err := s.emit(ctx, stored, domainevent.PayoutCreated{PayoutID: stored.ID, PayeeID: stored.PayeeID}); err != nil {
		return uuid.Nil(), err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "payout created", slog.String("payout_id", stored.ID.String()),
		slog.String("payee_id", stored.PayeeID.String()), slog.Int("items", len(items)), slog.Int64("amount", stored.Amount))
	return stored.ID, nil
}

// AdminApprovePayout implements PayoutService, the second eye (03 §6.1):
// the gate (403 before reading) → the payout (404) → authz on it (403
// authz.four_eyes_violation for its creator; fin only) → the
// re-authentication (403 identity.reauth_required / reauth_failed) → in
// one transaction: the payout locked, pending only (409), the eyes again
// on the locked row → the snapshotted account must still be the payee's
// approved one (409 ledger.bank_account_changed) → approved by, at (the
// DB CHECK refuses the creator too) → audit → payout.approved (O).
func (s *service) AdminApprovePayout(ctx context.Context, a authz.Actor, payoutID uuid.UUID, proof identity.ReauthProof) (PayoutView, error) {
	if err := s.adminGate(ctx, a, authz.LedgerPayoutRead); err != nil {
		return PayoutView{}, err
	}
	p, err := s.d.Payouts.GetPayout(ctx, payoutID, false)
	if err != nil {
		return PayoutView{}, err
	}
	if err := authz.Can(ctx, a, authz.LedgerPayoutApprove, payoutResource(p)); err != nil {
		return PayoutView{}, err
	}
	if s.d.Reauth == nil {
		return PayoutView{}, problem.ErrUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		// A refused proof is audited as denied; overload writes nothing (03 §6.2, BE-074).
		denied := audit.Event{Action: string(authz.LedgerPayoutApprove), OrgID: &p.OrgID, ResourceType: authz.TypePayout, ResourceID: &p.ID}.By(a)
		return PayoutView{}, audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Payout uuid.UUID
	}{"approve_payout", payoutID})
	if err != nil {
		return PayoutView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Payouts.GetPayout(ctx, payoutID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != StatusPendingApproval {
			return uuid.Nil(), ErrInvalidState
		}
		if err := authz.Can(ctx, a, authz.LedgerPayoutApprove, payoutResource(cur)); err != nil {
			return uuid.Nil(), err
		}
		acct, err := s.d.Payees.ApprovedAccount(ctx, cur.PayeeID)
		switch {
		case errors.Is(err, parties.ErrNoApprovedAccount):
			return uuid.Nil(), ErrBankAccountChanged
		case err != nil:
			return uuid.Nil(), err
		case acct.ID != cur.BankAccountID || acct.IBAN != cur.IBANSnapshot || acct.HolderName != cur.HolderSnapshot:
			return uuid.Nil(), ErrBankAccountChanged
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.ApprovedBy, next.ApprovedAt = StatusApproved, &by, &now
		stored, err := s.d.Payouts.TransitionPayout(ctx, next, StatusPendingApproval)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, payoutEvent(a, authz.LedgerPayoutApprove, stored, "",
			map[string]any{"from": string(cur.Status), audit.MetaReauth: kind})); err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.emit(ctx, stored, domainevent.PayoutApproved{PayoutID: stored.ID})
	})
	if err != nil {
		return PayoutView{}, translatePayout(err)
	}
	return s.reload(ctx, payoutID)
}

// AdminMarkPayoutPaid implements PayoutService (01 §6.3 "Payout ödendi"):
// authz (403) → bank reference and receipt required (422), the receipt a
// payout_receipt upload (422) → in one transaction: the payout locked,
// approved only (409) → ledger payout_paid (seller_payable(payee) +amount /
// koro_bank −amount; 409 ledger.negative_payee_balance) →
// orders.MarkLinesPaidOut → the receipt attached → paid by, at, with the
// reference and the receipt → audit → payout.paid (O). The deferred
// triggers re-check at COMMIT that the items sum to the amount and every
// line is marked paid out (payout_items_sum).
func (s *service) AdminMarkPayoutPaid(ctx context.Context, a authz.Actor, payoutID uuid.UUID, in MarkPaidInput) (PayoutView, error) {
	if err := s.adminGate(ctx, a, authz.LedgerPayoutMarkPaid); err != nil {
		return PayoutView{}, err
	}
	ref := strings.TrimSpace(in.BankReference)
	now := s.now()
	paidAt := now
	if in.PaidAt != nil {
		paidAt = payoutStamp(*in.PaidAt)
	}
	switch {
	case ref == "":
		return PayoutView{}, fieldErr("body.bank_reference", "is required: the reference of the transfer at the bank")
	case utf8.RuneCountInString(ref) > maxBankRefRunes:
		return PayoutView{}, fieldErr("body.bank_reference", "must be at most 100 characters")
	case in.ReceiptFileID == nil || *in.ReceiptFileID == uuid.Nil():
		return PayoutView{}, fieldErr("body.receipt_file_id", "is required: the bank receipt (dekont) of the transfer")
	case paidAt.After(now.Add(paidAtSkew)):
		return PayoutView{}, fieldErr("body.paid_at", "must not be in the future")
	}
	receipt := *in.ReceiptFileID
	// The hash takes paid_at as given: a retry without paid_at replays the
	// first call, whatever "now" is then.
	var givenPaidAt *time.Time
	if in.PaidAt != nil {
		givenPaidAt = &paidAt
	}
	f, err := s.d.Files.Get(ctx, receipt)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return PayoutView{}, fieldErr("body.receipt_file_id", "must be a payout_receipt upload")
	case err != nil:
		return PayoutView{}, err
	case f.Purpose != files.PurposePayoutReceipt:
		return PayoutView{}, fieldErr("body.receipt_file_id", "must be a payout_receipt upload")
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Payout  uuid.UUID
		Ref     string
		Receipt uuid.UUID
		PaidAt  *time.Time
	}{"mark_payout_paid", payoutID, ref, receipt, givenPaidAt})
	if err != nil {
		return PayoutView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		return s.markPaid(ctx, a, payoutID, ref, receipt, paidAt)
	})
	if err != nil {
		return PayoutView{}, translatePayout(err)
	}
	return s.reload(ctx, payoutID)
}

func (s *service) markPaid(ctx context.Context, a authz.Actor, payoutID uuid.UUID, ref string, receipt uuid.UUID, paidAt time.Time) (uuid.UUID, error) {
	cur, err := s.d.Payouts.GetPayout(ctx, payoutID, true)
	if err != nil {
		return uuid.Nil(), err
	}
	if cur.Status != StatusApproved {
		return uuid.Nil(), ErrInvalidState
	}
	items, err := s.d.Payouts.PayoutItems(ctx, cur.ID)
	if err != nil {
		return uuid.Nil(), err
	}
	lineIDs := make([]uuid.UUID, len(items))
	for i, it := range items {
		lineIDs[i] = it.OrderLineID
	}
	// Payouts exist only for money Koro collected (models A and A2; a
	// model C line is never payable): the model A template.
	t, err := PayoutPaid(ModelA, cur.ID, cur.PayeeID, cur.Amount, paidAt)
	if err != nil {
		return uuid.Nil(), err
	}
	if err := t.validate(); err != nil {
		return uuid.Nil(), err
	}
	txnID, err := s.post(ctx, t)
	if err != nil {
		return uuid.Nil(), err
	}
	if err := s.d.Orders.MarkLinesPaidOut(ctx, lineIDs, cur.ID); err != nil {
		return uuid.Nil(), err
	}
	if err := s.d.Files.Attach(ctx, a, receipt, files.Owner{Module: "ledger", EntityID: cur.ID, Purpose: files.PurposePayoutReceipt}); err != nil {
		return uuid.Nil(), err
	}
	by := a.UserID
	next := cur
	next.Status, next.PaidBy, next.PaidAt, next.BankReference, next.ReceiptFileID = StatusPaid, &by, &paidAt, &ref, &receipt
	stored, err := s.d.Payouts.TransitionPayout(ctx, next, StatusApproved)
	if err != nil {
		return uuid.Nil(), err
	}
	meta := map[string]any{"from": string(cur.Status), "transaction_id": txnID.String(), "receipt_file_id": receipt.String(),
		"items": len(items)}
	if cur.ApprovedAt != nil && paidAt.Before(*cur.ApprovedAt) {
		// The transfer is dated before the second eye approved it: recorded
		// (the money left the bank) and flagged for finance's review.
		meta["paid_before_approval"] = true
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "payout marked paid with a transfer dated before its approval",
			slog.String("payout_id", cur.ID.String()))
	}
	if err := s.d.Audit.Log(ctx, payoutEvent(a, authz.LedgerPayoutMarkPaid, stored, "", meta)); err != nil {
		return uuid.Nil(), err
	}
	if err := s.emit(ctx, stored, domainevent.PayoutPaid{PayoutID: stored.ID, PayeeID: stored.PayeeID, Amount: stored.Amount}); err != nil {
		return uuid.Nil(), err
	}
	return stored.ID, nil
}

// AdminMarkPayoutFailed implements PayoutService: authz (403) → the reason
// (422) → in one transaction: the payout locked, approved only (409: the
// transfer failed after the approval) → failed with the reason → its items
// deleted (the lines are payable again) → audit.
func (s *service) AdminMarkPayoutFailed(ctx context.Context, a authz.Actor, payoutID uuid.UUID, reason string) (PayoutView, error) {
	return s.release(ctx, a, authz.LedgerPayoutMarkFailed, payoutID, "body.failure_reason", reason,
		[]PayoutStatus{StatusApproved}, func(next *Payout, why string, _ time.Time) {
			next.Status, next.FailureReason = StatusFailed, &why
		})
}

// AdminCancelPayout implements PayoutService: authz (403) → the reason
// (422) → in one transaction: the payout locked, pending_approval or
// approved only (409; a paid payout is final) → cancelled at, with the
// reason → its items deleted → audit.
func (s *service) AdminCancelPayout(ctx context.Context, a authz.Actor, payoutID uuid.UUID, reason string) (PayoutView, error) {
	return s.release(ctx, a, authz.LedgerPayoutCancel, payoutID, "body.cancel_reason", reason,
		[]PayoutStatus{StatusPendingApproval, StatusApproved}, func(next *Payout, why string, now time.Time) {
			next.Status, next.CancelledAt, next.CancelReason = StatusCancelled, &now, &why
		})
}

// release ends a payout without money moving (failed, cancelled): its
// items are deleted after the transition (payout_items_guard allows that
// only for a failed or cancelled payout) and its lines are payable again.
func (s *service) release(ctx context.Context, a authz.Actor, act authz.Action, payoutID uuid.UUID, loc, reason string,
	from []PayoutStatus, apply func(next *Payout, why string, now time.Time)) (PayoutView, error) {
	if err := s.adminGate(ctx, a, act); err != nil {
		return PayoutView{}, err
	}
	why := strings.TrimSpace(reason)
	switch n := utf8.RuneCountInString(why); {
	case n == 0:
		return PayoutView{}, fieldErr(loc, "is required")
	case n > maxReasonRunes:
		return PayoutView{}, fieldErr(loc, "must be at most 500 characters")
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Payout uuid.UUID
		Reason string
	}{string(act), payoutID, why})
	if err != nil {
		return PayoutView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Payouts.GetPayout(ctx, payoutID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if !slices.Contains(from, cur.Status) {
			return uuid.Nil(), ErrInvalidState
		}
		next := cur
		apply(&next, why, s.now())
		stored, err := s.d.Payouts.TransitionPayout(ctx, next, cur.Status)
		if err != nil {
			return uuid.Nil(), err
		}
		n, err := s.d.Payouts.DeleteItems(ctx, stored.ID)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, payoutEvent(a, act, stored, why,
			map[string]any{"from": string(cur.Status), "released_items": n})); err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, nil
	})
	if err != nil {
		return PayoutView{}, translatePayout(err)
	}
	return s.reload(ctx, payoutID)
}

// reload reads a payout after a write for the response.
func (s *service) reload(ctx context.Context, id uuid.UUID) (PayoutView, error) {
	p, err := s.d.Payouts.GetPayout(ctx, id, false)
	if err != nil {
		return PayoutView{}, err
	}
	return s.view(ctx, p)
}
