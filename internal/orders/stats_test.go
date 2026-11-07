package orders_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/files"
	"koro/internal/orders"
)

// TestPurchaseStats_EvidenceStates (BE-189): the company panel's purchases
// carry the delivery evidence of an event's lines by the files' states: a
// clean (or not yet scanned) evidence counts as attached, a rejected one
// apart (the seller uploads it again); without the files module every
// attached evidence counts as attached.
func TestPurchaseStats_EvidenceStates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		noFiles            bool
		attached, rejected int
	}{
		{"by the files' states", false, 2, 1},
		{"without the files module", true, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, func(d *orders.Deps) {
				if tc.noFiles {
					d.Files = nil
				}
			})
			clean := e.evidenceFile(t, "club_a", "club_admin", nil)
			pending := e.evidenceFile(t, "club_a", "club_admin", func(f *files.File) { f.ScanStatus = files.ScanPending })
			rejected := e.evidenceFile(t, "club_a", "club_admin", func(f *files.File) { f.Status = files.StatusRejected })
			b, sl := buyerRef{org: org("company_a"), user: uuid.NewV7(), party: uuid.NewV7()}, sellerRef{org: org("club_a"), payee: e.payeeA}
			ev, placed := uuid.NewV7(), t0.Add(-time.Hour)
			offs := []offeringRef{{uuid.NewV7(), e.booth, uuid.NewV7()}, {uuid.NewV7(), e.booth, uuid.NewV7()},
				{uuid.NewV7(), e.booth, uuid.NewV7()}, {uuid.NewV7(), e.booth, uuid.NewV7()}}
			f := newFixture(b, placed, offs, []sellerRef{sl, sl, sl, sl})
			for i, id := range []*uuid.UUID{&clean, &pending, &rejected, nil} {
				f.lines[i].EventID, f.lines[i].FulfillmentEvidenceFileID = &ev, id
			}
			if err := e.store.InsertOrder(t.Context(), f.o, f.lines); err != nil {
				t.Fatal(err)
			}
			st, err := e.svc.(orders.Statistics).PurchaseStats(t.Context(), b.org, t0.AddDate(0, 0, -1), t0)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Events) != 1 || st.Events[0].Lines != 4 || st.Events[0].Evidence != tc.attached ||
				st.Events[0].EvidenceRejected != tc.rejected {
				t.Fatalf("events = %+v, want %d attached and %d rejected", st.Events, tc.attached, tc.rejected)
			}
		})
	}
}
