package billing_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/billing"
	"koro/internal/platform/civil"
	"koro/internal/platform/db"
)

// world creates the parent rows a store needs (FKs in PostgreSQL; plain
// IDs in memory) and runs writes in a transaction of the store's database.
type world struct {
	user  func(t *testing.T) uuid.UUID
	party func(t *testing.T) uuid.UUID
	org   func(t *testing.T, kind string) uuid.UUID
	// order returns a paid order with one line.
	order func(t *testing.T) (order, line uuid.UUID)
	file  func(t *testing.T) uuid.UUID
	tx    db.TxManager
}

func memWorld() world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	return world{user: id, party: id, file: id, org: func(*testing.T, string) uuid.UUID { return uuid.NewV7() },
		order: func(*testing.T) (uuid.UUID, uuid.UUID) { return uuid.NewV7(), uuid.NewV7() }, tx: db.NopTx{}}
}

// fixture is a planned service invoice of one order line.
type fixture struct {
	inv   billing.Invoice
	lines []billing.Line
}

// stale is a caller's timestamp the stores ignore: created_at and updated_at
// come from one clock, the database's (02 §0.1 [ts], BE-069).
var stale = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func newFixture(t *testing.T, w world) fixture {
	t.Helper()
	order, line := w.order(t)
	issuerOrg, recipientOrg := w.org(t, "club"), w.org(t, "company")
	inv := billing.Invoice{
		ID: uuid.NewV7(), OrderID: &order, Kind: billing.KindServiceSale, IssuerPartyID: w.party(t),
		RecipientPartyID: w.party(t), IssuerOrgID: &issuerOrg, RecipientOrgID: &recipientOrg,
		IssuerSnapshot: billing.Snapshot{Title: "Sözleşme Vakfı", TaxNo: "1234567890"}, RecipientSnapshot: billing.Snapshot{Title: "Alıcı A.Ş."},
		IssuingMode: billing.ModeSellerUploaded, DocumentType: billing.DocUnknown, Provider: billing.ProviderNone,
		Status: billing.StatusPlanned, Trigger: billing.TriggerOnPayment, Currency: "TRY", NetAmount: 1_000_000,
		VATAmount: 200_000, VATWithheldAmount: 60_000, TotalAmount: 1_200_000, PayableAmount: 1_140_000,
		IdempotencyKey: "service_sale:" + order.String() + ":" + line.String() + ":1", CreatedAt: stale, UpdatedAt: stale,
	}
	return fixture{inv: inv, lines: []billing.Line{{ID: uuid.NewV7(), InvoiceID: inv.ID, Position: 1, OrderLineID: &line,
		Description: "Stant", Quantity: 1, UnitPriceAmount: 1_000_000, NetAmount: 1_000_000, VATRateBP: 2000, VATAmount: 200_000,
		VATWithheldAmount: 60_000, TotalAmount: 1_200_000}}}
}

func insertIn(t *testing.T, s billing.Store, w world, f fixture) (billing.Invoice, bool) {
	t.Helper()
	var (
		out     billing.Invoice
		created bool
	)
	err := w.tx.WithTx(t.Context(), func(ctx context.Context) error {
		var err error
		out, created, err = s.InsertInvoice(ctx, f.inv, f.lines)
		return err
	})
	if err != nil {
		t.Fatalf("InsertInvoice: %v", err)
	}
	return out, created
}

func updateIn(ctx context.Context, s billing.Store, w world, next billing.Invoice, from billing.Status) (billing.Invoice, error) {
	var out billing.Invoice
	err := w.tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.UpdateInvoice(ctx, next, from)
		return err
	})
	return out, err
}

// uploaded returns inv with an upload by user.
func uploaded(inv billing.Invoice, no string, by, file uuid.UUID) billing.Invoice {
	d := civil.Date{Year: 2026, Month: time.October, Day: 13}
	inv.InvoiceNo, inv.IssueDate, inv.DocumentType, inv.PDFFileID, inv.UploadedBy = &no, &d, billing.DocEArsiv, &file, &by
	inv.PDFSHA256 = make([]byte, 32)
	return inv
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (billing.Store, world)) {
	t.Helper()
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

	t.Run("insert is idempotent by key", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		f := newFixture(t, w)
		stored, created := insertIn(t, s, w, f)
		if !created || stored.ID != f.inv.ID || stored.Status != billing.StatusPlanned || stored.IssuerSnapshot.Title != "Sözleşme Vakfı" {
			t.Fatalf("first insert = %+v, %v", stored, created)
		}
		// The store stamps both columns on insert, at once (BE-069).
		if stored.CreatedAt.IsZero() || stored.CreatedAt.Equal(stale) || !stored.UpdatedAt.Equal(stored.CreatedAt) {
			t.Fatalf("insert stamps created_at %v, updated_at %v; want one store stamp", stored.CreatedAt, stored.UpdatedAt)
		}
		again := f
		again.inv.ID = uuid.NewV7()
		dup, created := insertIn(t, s, w, again)
		if created || dup.ID != f.inv.ID {
			t.Fatalf("duplicate insert = %s, %v; want the first invoice", dup.ID, created)
		}
		ls, err := s.InvoiceLines(t.Context(), f.inv.ID)
		if err != nil || len(ls) != 1 || ls[0].Position != 1 || ls[0].TotalAmount != 1_200_000 {
			t.Fatalf("lines = %+v, %v", ls, err)
		}
		if _, err := s.GetInvoice(t.Context(), again.inv.ID, false); !errors.Is(err, billing.ErrNotFound) {
			t.Fatalf("the duplicate was stored: %v", err)
		}
		of, err := s.InvoicesOfOrderLine(t.Context(), *f.lines[0].OrderLineID)
		if err != nil || len(of) != 1 || of[0].ID != f.inv.ID {
			t.Fatalf("InvoicesOfOrderLine = %+v, %v", of, err)
		}
	})

	t.Run("upload, four eyes, issue, frozen, cancel", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		f := newFixture(t, w)
		inv, _ := insertIn(t, s, w, f)
		ctx := t.Context()
		uploader, verifier, file := w.user(t), w.user(t), w.file(t)
		if _, err := updateIn(ctx, s, w, uploaded(inv, "VKF1", uploader, file), billing.StatusMismatch); !errors.Is(err, billing.ErrInvalidState) {
			t.Fatalf("update from a wrong state = %v", err)
		}
		up, err := updateIn(ctx, s, w, uploaded(inv, "VKF1", uploader, file), billing.StatusPlanned)
		if err != nil || up.UploadedBy == nil || *up.UploadedBy != uploader || up.InvoiceNo == nil || *up.InvoiceNo != "VKF1" ||
			up.IssueDate == nil || up.IssueDate.Day != 13 || !up.CreatedAt.Equal(inv.CreatedAt) || up.UpdatedAt.Before(up.CreatedAt) {
			t.Fatalf("upload = %+v, %v", up, err)
		}
		now := at.Add(time.Hour)
		self := up
		self.Status, self.VerifiedBy, self.VerifiedAt, self.IssuedAt = billing.StatusIssued, &uploader, &now, &now
		if _, err := updateIn(ctx, s, w, self, billing.StatusPlanned); !errors.Is(err, authz.ErrFourEyes) {
			t.Fatalf("the uploader verified: %v (invoices_verifier_chk)", err)
		}
		half := up
		half.Status = billing.StatusIssued // no verifier, no issued_at
		if _, err := updateIn(ctx, s, w, half, billing.StatusPlanned); err == nil {
			t.Fatal("an issued invoice without its issue columns was stored (invoices_issued_chk)")
		}
		ok := up
		ok.Status, ok.VerifiedBy, ok.VerifiedAt, ok.IssuedAt = billing.StatusIssued, &verifier, &now, &now
		issued, err := updateIn(ctx, s, w, ok, billing.StatusPlanned)
		if err != nil || issued.Status != billing.StatusIssued || issued.IssuedAt == nil || !issued.IssuedAt.Equal(now) {
			t.Fatalf("issue = %+v, %v", issued, err)
		}
		renum := issued
		renum.Status = billing.StatusIssued
		renum.InvoiceNo = ptr("VKF2")
		if _, err := updateIn(ctx, s, w, renum, billing.StatusIssued); err == nil {
			t.Fatal("an issued invoice's number changed (invoices_frozen)")
		}
		cancelled := issued
		cancelled.Status = billing.StatusCancelled
		got, err := updateIn(ctx, s, w, cancelled, billing.StatusIssued)
		if err != nil || got.Status != billing.StatusCancelled || got.IssuedAt == nil {
			t.Fatalf("cancel of an issued invoice = %+v, %v", got, err)
		}
		by, err := s.InvoicesBy(ctx, verifier)
		if err != nil || len(by) != 1 || by[0].ID != inv.ID {
			t.Fatalf("InvoicesBy(verifier) = %d, %v", len(by), err)
		}
	})

	t.Run("an issuer's invoice number is unique", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		a := newFixture(t, w)
		b := newFixture(t, w)
		b.inv.IssuerPartyID = a.inv.IssuerPartyID
		ia, _ := insertIn(t, s, w, a)
		ib, _ := insertIn(t, s, w, b)
		if _, err := updateIn(t.Context(), s, w, uploaded(ia, "VKF7", w.user(t), w.file(t)), billing.StatusPlanned); err != nil {
			t.Fatal(err)
		}
		_, err := updateIn(t.Context(), s, w, uploaded(ib, "VKF7", w.user(t), w.file(t)), billing.StatusPlanned)
		if !errors.Is(err, billing.ErrInvoiceNoTaken) {
			t.Fatalf("a second VKF7 of the issuer = %v", err)
		}
	})

	t.Run("lists by org, direction, filter and cursor", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		a := newFixture(t, w)
		b := newFixture(t, w)
		b.inv.RecipientOrgID = a.inv.IssuerOrgID // a's issuer receives b
		b.inv.Kind = billing.KindPlatformFee
		insertIn(t, s, w, a)
		insertIn(t, s, w, b)
		org := *a.inv.IssuerOrgID
		ctx := t.Context()
		for _, c := range []struct {
			d    billing.Direction
			want []uuid.UUID
		}{
			{"", []uuid.UUID{b.inv.ID, a.inv.ID}}, {billing.DirectionIssued, []uuid.UUID{a.inv.ID}},
			{billing.DirectionReceived, []uuid.UUID{b.inv.ID}},
		} {
			rows, err := s.ListInvoices(ctx, billing.Filter{OrgID: &org, Direction: c.d}, nil, 10)
			if err != nil || len(rows) != len(c.want) {
				t.Fatalf("direction %q: %d, %v", c.d, len(rows), err)
			}
			for i := range rows {
				if rows[i].ID != c.want[i] {
					t.Fatalf("direction %q: order %v", c.d, rows)
				}
			}
		}
		rows, _ := s.ListInvoices(ctx, billing.Filter{}, nil, 1)
		if len(rows) != 1 || rows[0].ID != b.inv.ID {
			t.Fatalf("first page = %+v", rows)
		}
		rows, _ = s.ListInvoices(ctx, billing.Filter{OrgID: &org}, &billing.Cursor{CreatedAt: rows[0].CreatedAt, ID: rows[0].ID}, 10)
		if len(rows) != 1 || rows[0].ID != a.inv.ID {
			t.Fatalf("after the cursor = %+v", rows)
		}
		if rows, _ := s.ListInvoices(ctx, billing.Filter{Kind: billing.KindPlatformFee, OrderID: b.inv.OrderID}, nil, 10); len(rows) != 1 {
			t.Fatalf("kind + order filter = %d", len(rows))
		}
		if rows, _ := s.ListInvoices(ctx, billing.Filter{Status: billing.StatusIssued, OrgID: &org}, nil, 10); len(rows) != 0 {
			t.Fatalf("status filter = %d", len(rows))
		}
	})

	t.Run("integration credentials", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		put := func(c billing.Credential) billing.Credential {
			var out billing.Credential
			if err := w.tx.WithTx(ctx, func(ctx context.Context) error {
				var err error
				out, err = s.PutCredential(ctx, c)
				return err
			}); err != nil {
				t.Fatalf("PutCredential: %v", err)
			}
			return out
		}
		exp := at.Add(time.Hour)
		first := put(billing.Credential{Provider: "parasut", ExternalAccount: ptr("acct-1"), AccessTokenEnc: []byte("sealed-1"),
			ExpiresAt: &exp, RotatedAt: &at})
		if first.Provider != "parasut" || string(first.AccessTokenEnc) != "sealed-1" || first.RefreshTokenEnc != nil {
			t.Fatalf("first put = %+v", first)
		}
		second := put(billing.Credential{Provider: "parasut", RefreshTokenEnc: []byte("sealed-2"), RotatedAt: &exp})
		if second.ExternalAccount != nil || second.AccessTokenEnc != nil || string(second.RefreshTokenEnc) != "sealed-2" {
			t.Fatalf("replace = %+v", second)
		}
		rows, err := s.Credentials(ctx)
		if err != nil || len(rows) < 1 {
			t.Fatalf("Credentials = %d, %v", len(rows), err)
		}
	})
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) (billing.Store, world) {
		return billing.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}
