package billing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"

	"koro/internal/authz"
)

// MemStore is the in-memory Store of service, wiring and matrix tests (05
// §13.2). It applies the rules of 000116 the service relies on: the unique
// idempotency key and invoice number, the four-eyes CHECK of the
// verification, the issued state's columns and the frozen document.
type MemStore struct {
	now func() time.Time

	mu    sync.Mutex
	inv   map[uuid.UUID]Invoice
	lines map[uuid.UUID][]Line
	creds map[string]Credential
}

// NewMemStore returns an empty MemStore stamping rows with now.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{now: now, inv: map[uuid.UUID]Invoice{}, lines: map[uuid.UUID][]Line{}, creds: map[string]Credential{}}
}

var _ Store = (*MemStore)(nil)

func cloneInvoice(i Invoice) Invoice {
	i.PDFSHA256 = bytes.Clone(i.PDFSHA256)
	return i
}

// checkRow applies the CHECKs of invoices the service could break.
func checkRow(i Invoice) error {
	switch {
	case i.VerifiedBy != nil && (i.UploadedBy == nil || *i.VerifiedBy == *i.UploadedBy):
		return authz.ErrFourEyes // invoices_verifier_chk
	case (i.VerifiedBy == nil) != (i.VerifiedAt == nil):
		return ErrInvalidState // invoices_verified_pair_chk
	case i.Status == StatusIssued && (i.InvoiceNo == nil || i.IssueDate == nil || i.PDFFileID == nil || i.IssuedAt == nil):
		return ErrInvalidState // invoices_issued_chk
	case i.IssuedAt != nil && i.Status != StatusIssued && i.Status != StatusCancelled:
		return ErrInvalidState // invoices_issued_at_chk
	case i.IssuingMode.Uploaded() && i.Status == StatusIssued && (i.UploadedBy == nil || i.VerifiedBy == nil):
		return ErrInvalidState // invoices_uploaded_chk, invoices_manual_verified_chk
	case (i.Kind == KindCreditNote) != (i.RefersToInvoiceID != nil):
		return ErrInvalidState // invoices_credit_note_chk
	case i.IssuerPartyID == i.RecipientPartyID:
		return errors.New("billing: invoices_parties_chk: issuer = recipient")
	case i.TotalAmount != i.NetAmount+i.VATAmount || i.PayableAmount != i.TotalAmount-i.VATWithheldAmount:
		return errors.New("billing: invoices_total_chk / invoices_payable_chk")
	}
	return nil
}

// frozen is invoices_frozen: once issued_at is set the document stays.
func frozen(old, next Invoice) error {
	if old.IssuedAt == nil {
		return nil
	}
	same := func(a, b *uuid.UUID) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	sameStr := func(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	sameTime := func(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }
	if !sameStr(old.InvoiceNo, next.InvoiceNo) || !same(old.ETTN, next.ETTN) || !same(old.PDFFileID, next.PDFFileID) ||
		!bytes.Equal(old.PDFSHA256, next.PDFSHA256) || !same(old.UploadedBy, next.UploadedBy) ||
		!same(old.VerifiedBy, next.VerifiedBy) || !sameTime(old.VerifiedAt, next.VerifiedAt) ||
		!sameTime(old.IssuedAt, next.IssuedAt) || (old.IssueDate == nil) != (next.IssueDate == nil) ||
		(old.IssueDate != nil && *old.IssueDate != *next.IssueDate) {
		return errors.New("billing: UPDATE of invoices is not allowed: the document is frozen once issued_at is set")
	}
	return nil
}

// noTaken reports whether another invoice of the issuer has the number.
func (m *MemStore) noTaken(i Invoice) bool {
	if i.InvoiceNo == nil {
		return false
	}
	for _, o := range m.inv {
		if o.ID != i.ID && o.IssuerPartyID == i.IssuerPartyID && o.InvoiceNo != nil && *o.InvoiceNo == *i.InvoiceNo {
			return true
		}
	}
	return false
}

// InsertInvoice implements Store.
func (m *MemStore) InsertInvoice(_ context.Context, inv Invoice, lines []Line) (Invoice, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.inv {
		if o.IdempotencyKey == inv.IdempotencyKey {
			return cloneInvoice(o), false, nil
		}
	}
	if err := checkRow(inv); err != nil {
		return Invoice{}, false, err
	}
	if m.noTaken(inv) {
		return Invoice{}, false, ErrInvoiceNoTaken
	}
	// The store owns both timestamps, as the column defaults of gormStore
	// (one clock, BE-069): a caller's values are ignored.
	now := stamp(m.now())
	inv.CreatedAt, inv.UpdatedAt = now, now
	m.inv[inv.ID] = cloneInvoice(inv)
	m.lines[inv.ID] = slices.Clone(lines)
	return cloneInvoice(inv), true, nil
}

// GetInvoice implements Store.
func (m *MemStore) GetInvoice(_ context.Context, id uuid.UUID, _ bool) (Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.inv[id]
	if !ok {
		return Invoice{}, ErrNotFound
	}
	return cloneInvoice(i), nil
}

// InvoiceLines implements Store.
func (m *MemStore) InvoiceLines(_ context.Context, invoiceID uuid.UUID) ([]Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.lines[invoiceID])
	slices.SortFunc(out, func(a, b Line) int { return int(a.Position) - int(b.Position) })
	return out, nil
}

// UpdateInvoice implements Store: only the running columns change.
func (m *MemStore) UpdateInvoice(_ context.Context, next Invoice, from Status) (Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.inv[next.ID]
	if !ok {
		return Invoice{}, ErrNotFound
	}
	if cur.Status != from {
		return Invoice{}, ErrInvalidState
	}
	upd := cur
	upd.Status, upd.DocumentType, upd.InvoiceNo, upd.ETTN, upd.IssueDate = next.Status, next.DocumentType, next.InvoiceNo,
		next.ETTN, next.IssueDate
	upd.PDFFileID, upd.PDFSHA256, upd.UploadedBy, upd.VerifiedBy, upd.VerifiedAt = next.PDFFileID, bytes.Clone(next.PDFSHA256),
		next.UploadedBy, next.VerifiedBy, next.VerifiedAt
	upd.IssuedAt, upd.LastError = next.IssuedAt, next.LastError
	if err := frozen(cur, upd); err != nil {
		return Invoice{}, err
	}
	if err := checkRow(upd); err != nil {
		return Invoice{}, err
	}
	if m.noTaken(upd) {
		return Invoice{}, ErrInvoiceNoTaken
	}
	if upd.UpdatedAt = stamp(m.now()); upd.UpdatedAt.Before(cur.UpdatedAt) {
		upd.UpdatedAt = cur.UpdatedAt // a clock set back in a test: never before the last stamp
	}
	m.inv[upd.ID] = cloneInvoice(upd)
	return cloneInvoice(upd), nil
}

// sorted returns the invoices matching keep, newest first.
func (m *MemStore) sorted(keep func(Invoice) bool, desc bool) []Invoice {
	var out []Invoice
	for _, i := range m.inv {
		if keep(i) {
			out = append(out, cloneInvoice(i))
		}
	}
	slices.SortFunc(out, func(a, b Invoice) int {
		c := a.CreatedAt.Compare(b.CreatedAt)
		if c == 0 {
			c = bytes.Compare(a.ID[:], b.ID[:])
		}
		if desc {
			return -c
		}
		return c
	})
	return out
}

// ListInvoices implements Store.
func (m *MemStore) ListInvoices(_ context.Context, f Filter, after *Cursor, limit int) ([]Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	is := func(p *uuid.UUID, id uuid.UUID) bool { return p != nil && *p == id }
	out := m.sorted(func(i Invoice) bool {
		if f.OrgID != nil {
			issued, received := is(i.IssuerOrgID, *f.OrgID), is(i.RecipientOrgID, *f.OrgID)
			switch f.Direction {
			case DirectionIssued:
				if !issued {
					return false
				}
			case DirectionReceived:
				if !received {
					return false
				}
			default:
				if !issued && !received {
					return false
				}
			}
		}
		switch {
		case f.Kind != "" && i.Kind != f.Kind, f.Status != "" && i.Status != f.Status,
			f.OrderID != nil && !is(i.OrderID, *f.OrderID), f.PayoutID != nil && !is(i.PayoutID, *f.PayoutID):
			return false
		case after != nil:
			c := i.CreatedAt.Compare(after.CreatedAt)
			return c < 0 || (c == 0 && bytes.Compare(i.ID[:], after.ID[:]) < 0)
		}
		return true
	}, true)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// InvoicesOfOrderLine implements Store.
func (m *MemStore) InvoicesOfOrderLine(_ context.Context, orderLineID uuid.UUID) ([]Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sorted(func(i Invoice) bool {
		return slices.ContainsFunc(m.lines[i.ID], func(l Line) bool { return l.OrderLineID != nil && *l.OrderLineID == orderLineID })
	}, false), nil
}

// InvoicesBy implements Store.
func (m *MemStore) InvoicesBy(_ context.Context, userID uuid.UUID) ([]Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sorted(func(i Invoice) bool {
		return (i.UploadedBy != nil && *i.UploadedBy == userID) || (i.VerifiedBy != nil && *i.VerifiedBy == userID)
	}, false), nil
}

// Credentials implements Store.
func (m *MemStore) Credentials(_ context.Context) ([]Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Credential, 0, len(m.creds))
	for _, c := range m.creds {
		out = append(out, cloneCredential(c))
	}
	slices.SortFunc(out, func(a, b Credential) int {
		switch {
		case a.Provider < b.Provider:
			return -1
		case a.Provider > b.Provider:
			return 1
		}
		return 0
	})
	return out, nil
}

// PutCredential implements Store.
func (m *MemStore) PutCredential(_ context.Context, c Credential) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.Contains(integrationProviders, c.Provider) {
		return Credential{}, fmt.Errorf("billing: integration_credentials_provider_chk: %q", c.Provider)
	}
	c.UpdatedAt = stamp(m.now())
	m.creds[c.Provider] = cloneCredential(c)
	return cloneCredential(c), nil
}

func cloneCredential(c Credential) Credential {
	c.AccessTokenEnc, c.RefreshTokenEnc = bytes.Clone(c.AccessTokenEnc), bytes.Clone(c.RefreshTokenEnc)
	return c
}
