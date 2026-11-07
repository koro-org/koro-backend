package stats_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/events"
	"koro/internal/orders"
	"koro/internal/stats"
)

// TestCompany_SponsorshipResults (BE-189; uyum denetimi #73): the company
// panel carries its orders of the range by status, the delivery of their
// lines and each event they are bound to — its title, start, organiser,
// orders, delivery and evidence — in the order orders gives; the
// participation of a held event is an estimate (a small group has no
// number), never for a sensitive event or one not held yet; an event that
// cannot be read keeps its counts.
func TestCompany_SponsorshipResults(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	club, sens := authztest.ID("club_a"), authztest.ID("club_sensitive")
	held, sensitive, upcoming, gone, small := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	f.summaries[held] = events.EventSummary{ID: held, OrgID: club, Title: "Kariyer Günleri", Status: events.StatusCompleted,
		StartsAt: now.AddDate(0, 0, -10)}
	f.summaries[sensitive] = events.EventSummary{ID: sensitive, OrgID: sens, Title: "Buluşma", Status: events.StatusPublished,
		StartsAt: now.AddDate(0, 0, -3), Sensitive: true}
	f.summaries[upcoming] = events.EventSummary{ID: upcoming, OrgID: club, Title: "Bahar Şenliği", Status: events.StatusPublished,
		StartsAt: now.AddDate(0, 0, 20)}
	f.summaries[small] = events.EventSummary{ID: small, OrgID: club, Title: "Atölye", Status: events.StatusPublished,
		StartsAt: now.Add(-time.Hour)}
	f.figures = []events.EventFigures{{EventID: held, OrgID: club, Registered: 137, CheckedIn: 42},
		{EventID: sensitive, OrgID: sens, Registered: 60, CheckedIn: 30}, {EventID: small, OrgID: club, Registered: 12, CheckedIn: 7}}
	placed := now.AddDate(0, 0, -12)
	ev := func(id, seller uuid.UUID, st map[orders.Status]int, ful map[orders.Fulfillment]int, attached, rejected int) orders.EventPurchases {
		n, l := 0, 0
		for _, c := range st {
			n += c
		}
		for _, c := range ful {
			l += c
		}
		return orders.EventPurchases{EventID: id, SellerOrgID: seller, Orders: n, ByStatus: st, Lines: l, ByFulfillment: ful,
			LastPlacedAt: placed, Evidence: attached, EvidenceRejected: rejected}
	}
	f.purchases = orders.PurchaseStats{Orders: 6,
		ByStatus: map[orders.Status]int{orders.StatusCompleted: 2, orders.StatusPaid: 3, orders.StatusAwaitingPayment: 1}, Lines: 7,
		ByFulfillment: map[orders.Fulfillment]int{orders.FulfillFulfilled: 2, orders.FulfillMarked: 2, orders.FulfillPending: 3},
		EventCount:    6, Events: []orders.EventPurchases{
			ev(held, club, map[orders.Status]int{orders.StatusCompleted: 2}, map[orders.Fulfillment]int{orders.FulfillFulfilled: 2}, 2, 0),
			ev(sensitive, sens, map[orders.Status]int{orders.StatusPaid: 1}, map[orders.Fulfillment]int{orders.FulfillMarked: 1}, 0, 1),
			ev(upcoming, club, map[orders.Status]int{orders.StatusAwaitingPayment: 1}, map[orders.Fulfillment]int{orders.FulfillPending: 1}, 0, 0),
			ev(gone, club, map[orders.Status]int{orders.StatusPaid: 1}, map[orders.Fulfillment]int{orders.FulfillPending: 1}, 0, 0),
			ev(small, club, map[orders.Status]int{orders.StatusPaid: 1}, map[orders.Fulfillment]int{orders.FulfillMarked: 1}, 1, 0),
		}}
	st, err := svc.Company(t.Context(), persona(t, "company_admin"), authztest.ID("company_a"), stats.RangeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	sp := st.Sponsorship
	if sp.Orders != 6 || len(sp.ByStatus) != 3 || sp.ByStatus[0] != (stats.KeyCount{Key: "awaiting_payment", Count: 1}) ||
		sp.Lines != 7 || sp.Delivered != 2 || sp.AwaitingConfirmation != 2 || sp.EventCount != 6 || len(sp.Events) != 5 {
		t.Fatalf("sponsorship = %+v", sp)
	}
	h := sp.Events[0]
	if h.EventID != held.String() || h.Title != "Kariyer Günleri" || h.StartsAt == nil || !h.StartsAt.Equal(now.AddDate(0, 0, -10)) ||
		h.Status != "completed" || !h.Held || h.OrganizerID != club.String() || h.OrganizerName != "club_a" || h.OrganizerKind != "club" ||
		h.Orders != 2 || h.Delivered != 2 || !h.HasEvidence || h.EvidenceAttached != 2 || !h.LastOrderedAt.Equal(placed) {
		t.Fatalf("held event = %+v", h)
	}
	if p := h.Participation; p == nil || *p.Registered.Count != 100 || *p.CheckedIn.Count != 40 {
		t.Fatalf("held event participation = %+v", p)
	}
	if s := sp.Events[1]; !s.Held || s.Participation != nil || s.HasEvidence || s.EvidenceRejected != 1 {
		t.Fatalf("sensitive event = %+v", s)
	}
	if u := sp.Events[2]; u.Held || u.Participation != nil || u.Status != "published" {
		t.Fatalf("upcoming event = %+v", u)
	}
	if g := sp.Events[3]; g.EventID != gone.String() || g.Title != "" || g.StartsAt != nil || g.Status != "" || g.Held ||
		g.Participation != nil || g.Orders != 1 || g.OrganizerName != "club_a" {
		t.Fatalf("unreadable event = %+v", g)
	}
	if p := sp.Events[4].Participation; p == nil || *p.Registered.Count != 10 || p.CheckedIn.Count != nil || !p.CheckedIn.BelowThreshold {
		t.Fatalf("small event participation = %+v", p)
	}
	raw, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"amount", "buyer", "user", "email", "evidence_file"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("sponsorship answer carries %q: %s", leak, raw)
		}
	}
}

// TestCompany_SponsorshipIsTheCompanysOwn (BE-189): the purchases are read
// for the org of the path once the caller may read its panel, over the
// panel's range; another company's member, a club admin and a company's
// other kind of org never reach them; no order is an empty section
// (arrays, not null).
func TestCompany_SponsorshipIsTheCompanysOwn(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	st, err := svc.Company(t.Context(), persona(t, "company_member"), authztest.ID("company_a"),
		stats.RangeQuery{From: "2026-10-01", To: "2026-10-31"})
	if err != nil {
		t.Fatal(err)
	}
	if f.buyerAsk != authztest.ID("company_a") || !f.from.Equal(time.Date(2026, 9, 30, 21, 0, 0, 0, time.UTC)) ||
		!f.to.Equal(time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("PurchaseStats asked %s [%s, %s)", f.buyerAsk, f.from, f.to)
	}
	sp := st.Sponsorship
	if sp.Orders != 0 || sp.ByStatus == nil || sp.ByFulfillment == nil || sp.Events == nil || len(sp.Events) != 0 {
		t.Fatalf("empty sponsorship = %+v", sp)
	}
	if raw, _ := json.Marshal(sp); !strings.Contains(string(raw), `"events":[]`) || !strings.Contains(string(raw), `"by_status":[]`) {
		t.Fatalf("empty sponsorship JSON = %s", raw)
	}
	for _, tc := range []struct {
		persona, org string
		status       int
	}{
		{"other_company_member", "company_a", http.StatusNotFound},
		{"club_admin", "company_a", http.StatusNotFound},
		{"company_admin", "company_b", http.StatusNotFound},
		{"university_rep", "uni_a", http.StatusForbidden},
	} {
		f.buyerAsk = uuid.Nil()
		_, err := svc.Company(t.Context(), persona(t, tc.persona), authztest.ID(tc.org), stats.RangeQuery{})
		wantStatus(t, err, tc.status)
		if f.buyerAsk != uuid.Nil() {
			t.Fatalf("%s on %s: purchases of %s read", tc.persona, tc.org, f.buyerAsk)
		}
	}
}
