package orders

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"
	"uuid"
)

// SalesStats implements StatsStore.
func (m *MemStore) SalesStats(_ context.Context, sellerOrgID uuid.UUID, from, to time.Time) (SalesStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sold := map[uuid.UUID]bool{}
	for _, l := range m.lines {
		if l.SellerOrgID == sellerOrgID {
			sold[l.OrderID] = true
		}
	}
	st := SalesStats{ByStatus: map[Status]int{}}
	for id := range sold {
		o, ok := m.orders[id]
		if !ok || o.PlacedAt.Before(from) || !o.PlacedAt.Before(to) {
			continue
		}
		st.ByStatus[o.Status]++
		st.Orders++
	}
	return st, nil
}

// PurchaseStats implements StatsStore.
func (m *MemStore) PurchaseStats(_ context.Context, buyerOrgID uuid.UUID, from, to time.Time, limit int) (PurchaseStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := PurchaseStats{ByStatus: map[Status]int{}, ByFulfillment: map[Fulfillment]int{}, Events: []EventPurchases{}}
	in := map[uuid.UUID]Order{}
	for id, o := range m.orders {
		if o.OrgID != buyerOrgID || o.PlacedAt.Before(from) || !o.PlacedAt.Before(to) {
			continue
		}
		in[id] = o
		st.ByStatus[o.Status]++
		st.Orders++
	}
	type key struct{ event, seller uuid.UUID }
	byEvent := map[key]*EventPurchases{}
	orderSeen := map[key]map[uuid.UUID]bool{}
	for _, l := range m.lines {
		o, ok := in[l.OrderID]
		if !ok || l.OrgID != buyerOrgID || l.Dropped() {
			continue
		}
		if l.PayeeID != nil {
			st.ByFulfillment[l.FulfillmentStatus]++
			st.Lines++
		}
		if l.EventID == nil {
			continue
		}
		k := key{*l.EventID, l.SellerOrgID}
		e := byEvent[k]
		if e == nil {
			e = &EventPurchases{EventID: k.event, SellerOrgID: k.seller, ByStatus: map[Status]int{},
				ByFulfillment: map[Fulfillment]int{}, EvidenceFileIDs: []uuid.UUID{}}
			byEvent[k], orderSeen[k] = e, map[uuid.UUID]bool{}
		}
		if !orderSeen[k][o.ID] {
			orderSeen[k][o.ID] = true
			e.Orders++
			e.ByStatus[o.Status]++
		}
		e.Lines++
		e.ByFulfillment[l.FulfillmentStatus]++
		if l.FulfillmentEvidenceFileID != nil {
			e.EvidenceFileIDs = append(e.EvidenceFileIDs, *l.FulfillmentEvidenceFileID)
		}
		if o.PlacedAt.After(e.LastPlacedAt) {
			e.LastPlacedAt = o.PlacedAt
		}
	}
	for _, e := range byEvent {
		slices.SortFunc(e.EvidenceFileIDs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		st.Events = append(st.Events, *e)
	}
	slices.SortFunc(st.Events, func(a, b EventPurchases) int {
		return cmp.Or(b.LastPlacedAt.Compare(a.LastPlacedAt), strings.Compare(a.EventID.String(), b.EventID.String()),
			strings.Compare(a.SellerOrgID.String(), b.SellerOrgID.String()))
	})
	st.EventCount = len(st.Events)
	if len(st.Events) > limit {
		st.Events = st.Events[:limit]
	}
	return st, nil
}
