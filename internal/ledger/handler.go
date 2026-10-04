package ledger

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
)

// Register adds the ledger operations of BE-039 (04 §2.17:
// adminListLedgerTransactions) to api. svc may be nil (`koro openapi`):
// handlers only touch it when called. The payee balance and payout
// endpoints come with BE-044.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	tags := []string{"ledger"}
	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminListLedgerTransactions", Method: http.MethodGet, Path: "/admin/ledger/transactions", Tags: tags,
		Summary: "List ledger transactions with their entries",
		Description: "The double-entry journal, newest occurred_at first. Amounts are kuruş; debit is positive, " +
			"credit negative; the entries of a transaction sum to zero. Filters: kind, ref_type, ref_id and an " +
			"occurred_at window.",
	}, op.Meta{Action: string(authz.LedgerJournalList), Surface: op.SurfaceAdmin}), h.list)
}

type handler struct{ svc Service }

// sortJournal is the only order of the journal (cursor fingerprint, 04 §1.6).
const sortJournal = "-occurred_at"

// ---- DTOs

// LedgerEntryDTO is one entry of a ledger transaction.
type LedgerEntryDTO struct {
	EntryID     string `json:"entry_id" format:"uuid"`
	AccountCode string `json:"account_code" doc:"Chart of accounts code (02 §21.1)"`
	PayeeID     string `json:"payee_id,omitempty" format:"uuid" doc:"Set on per-payee accounts (seller_payable)"`
	OrderLineID string `json:"order_line_id,omitempty" format:"uuid"`
	Amount      int64  `json:"amount" doc:"Kuruş; debit positive, credit negative"`
	Currency    string `json:"currency" enum:"TRY"`
}

// LedgerTransactionDTO is one item of adminListLedgerTransactions (04 §2.17).
type LedgerTransactionDTO struct {
	TransactionID string           `json:"transaction_id" format:"uuid"`
	Kind          string           `json:"kind" enum:"payment_received,order_allocated,payout_paid,refund_completed,fee,adjustment"`
	RefType       string           `json:"ref_type" enum:"payment,order,payout,refund,manual"`
	RefID         string           `json:"ref_id" format:"uuid"`
	OccurredAt    time.Time        `json:"occurred_at"`
	Memo          string           `json:"memo,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	Entries       []LedgerEntryDTO `json:"entries"`
}

// LedgerTransactionList is the body of adminListLedgerTransactions.
type LedgerTransactionList struct {
	Items      []LedgerTransactionDTO `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

func toTransactionDTO(p Posted) LedgerTransactionDTO {
	d := LedgerTransactionDTO{
		TransactionID: p.ID.String(), Kind: string(p.Kind), RefType: string(p.RefType), RefID: p.RefID.String(),
		OccurredAt: p.OccurredAt.UTC(), CreatedAt: p.CreatedAt.UTC(), Entries: make([]LedgerEntryDTO, len(p.Entries)),
	}
	if p.Memo != nil {
		d.Memo = *p.Memo
	}
	for i, e := range p.Entries {
		ed := LedgerEntryDTO{EntryID: e.ID.String(), AccountCode: string(e.Account), Amount: e.Amount, Currency: Currency}
		if e.PayeeID != nil {
			ed.PayeeID = e.PayeeID.String()
		}
		if e.OrderLineID != nil {
			ed.OrderLineID = e.OrderLineID.String()
		}
		d.Entries[i] = ed
	}
	return d
}

// ---- adminListLedgerTransactions

type listInput struct {
	page.Params
	Kind         string `query:"kind" enum:"payment_received,order_allocated,payout_paid,refund_completed,fee,adjustment"`
	RefType      string `query:"ref_type" enum:"payment,order,payout,refund,manual"`
	RefID        string `query:"ref_id" format:"uuid" doc:"The referenced record (payment, order, payout, refund, reversed transaction)"`
	OccurredFrom string `query:"occurred_from" maxLength:"40" doc:"RFC 3339 time or YYYY-MM-DD (start of that day, Europe/Istanbul)"`
	OccurredTo   string `query:"occurred_to" maxLength:"40" doc:"RFC 3339 time or YYYY-MM-DD (end of that day, Europe/Istanbul); inclusive"`
}

type listOutput struct{ Body LedgerTransactionList }

func (h handler) list(ctx context.Context, in *listInput) (*listOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListLedgerTransactions", in.Kind, in.RefType, in.RefID, in.OccurredFrom, in.OccurredTo)
	p, err := h.svc.ListTransactions(ctx, a, in.query(fp))
	if err != nil {
		return nil, err
	}
	out := &listOutput{Body: LedgerTransactionList{Items: make([]LedgerTransactionDTO, len(p.Items))}}
	for i, t := range p.Items {
		out.Body.Items[i] = toTransactionDTO(t)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortJournal, fp, page.TimeVal(p.Next.OccurredAt), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

// query parses the parameters. A malformed one is not returned here: the
// service reports it after authorisation (JournalQuery.invalid; 03 §7.2).
func (in *listInput) query(fp string) JournalQuery {
	q := JournalQuery{Filter: JournalFilter{Kind: Kind(in.Kind), RefType: RefType(in.RefType)}, Limit: in.Limit}
	if in.RefID != "" {
		id, err := uuid.Parse(in.RefID)
		if err != nil {
			q.invalid = fieldErr("query.ref_id", "must be a UUID")
			return q
		}
		q.Filter.RefID = &id
	}
	for _, b := range []struct {
		raw, loc string
		end      bool
		dst      **time.Time
	}{
		{in.OccurredFrom, "query.occurred_from", false, &q.Filter.From},
		{in.OccurredTo, "query.occurred_to", true, &q.Filter.To},
	} {
		if b.raw == "" {
			continue
		}
		t, err := parseBound(b.raw, b.end)
		if err != nil {
			q.invalid = fieldErr(b.loc, "must be an RFC 3339 time or a YYYY-MM-DD date")
			return q
		}
		*b.dst = &t
	}
	after, err := afterOf(in.Cursor, fp)
	if err != nil {
		q.invalid = err
		return q
	}
	q.After = after
	return q
}

// afterOf decodes a journal cursor (occurred_at, id).
func afterOf(cursor, fp string) (*JournalCursor, error) {
	c, err := page.Decode(cursor, sortJournal, fp, 2)
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
	return &JournalCursor{OccurredAt: at, ID: id}, nil
}

// parseBound parses an occurred_from/occurred_to bound (04 §1.7: RFC 3339,
// or a date meaning the start/end of that Europe/Istanbul day); end bounds
// become exclusive.
func parseBound(s string, end bool) (time.Time, error) {
	if len(s) == len("2006-01-02") {
		d, err := civil.Parse(s)
		if err != nil {
			return time.Time{}, err
		}
		if end {
			d = d.AddDays(1)
		}
		return d.In(clock.Istanbul()).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	if end {
		t = t.Add(time.Microsecond) // inclusive at the database's precision
	}
	return t.UTC(), nil
}
