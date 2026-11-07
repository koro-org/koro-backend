package orders

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// SalesStats implements StatsStore.
//
// cross-org: keyed by the seller (seller_org_id), like ListSales; the
// buyers' orders are counted, never read out.
func (s gormStore) SalesStats(ctx context.Context, sellerOrgID uuid.UUID, from, to time.Time) (SalesStats, error) {
	var rows []struct {
		Status string
		N      int
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT o.status, count(*) AS n FROM orders o
	  WHERE o.placed_at >= ? AND o.placed_at < ?
	    AND o.id IN (SELECT l.order_id FROM order_lines l WHERE l.seller_org_id = ?)
	  GROUP BY o.status`, from, to, sellerOrgID).Scan(&rows).Error
	if err != nil {
		return SalesStats{}, fmt.Errorf("orders: sales stats: %w", err)
	}
	st := SalesStats{ByStatus: map[Status]int{}}
	for _, r := range rows {
		st.ByStatus[Status(r.Status)] = r.N
		st.Orders += r.N
	}
	return st, nil
}

// purchaseScope is the WHERE of PurchaseStats: the buyer's orders placed
// in the range and their lines that did not drop out (BE-134).
const purchaseScope = `FROM order_lines l JOIN orders o ON o.id = l.order_id
	  WHERE o.org_id = @org_id AND l.org_id = @org_id AND o.placed_at >= @from AND o.placed_at < @to
	    AND l.dropped_at IS NULL`

// PurchaseStats implements StatsStore: the buyer's orders by status, its
// seller lines by fulfilment, then up to limit events (latest placement
// first) with their orders by status and lines by fulfilment.
func (s gormStore) PurchaseStats(ctx context.Context, buyerOrgID uuid.UUID, from, to time.Time, limit int) (PurchaseStats, error) {
	conn := db.Conn(ctx, s.orm)
	args := map[string]any{"org_id": buyerOrgID, "from": from, "to": to, "limit": limit}
	fail := func(err error) (PurchaseStats, error) {
		return PurchaseStats{}, fmt.Errorf("orders: purchase stats: %w", err)
	}
	type count struct {
		Grp string
		N   int
	}
	st := PurchaseStats{ByStatus: map[Status]int{}, ByFulfillment: map[Fulfillment]int{}, Events: []EventPurchases{}}
	var byStatus []count
	if err := conn.Raw(`SELECT o.status AS grp, count(*) AS n FROM orders o
	  WHERE o.org_id = @org_id AND o.placed_at >= @from AND o.placed_at < @to
	  GROUP BY o.status`, args).Scan(&byStatus).Error; err != nil {
		return fail(err)
	}
	for _, r := range byStatus {
		st.ByStatus[Status(r.Grp)] = r.N
		st.Orders += r.N
	}
	var byFulfil []count
	if err := conn.Raw(`SELECT l.fulfillment_status AS grp, count(*) AS n `+purchaseScope+`
	    AND l.payee_id IS NOT NULL
	  GROUP BY l.fulfillment_status`, args).Scan(&byFulfil).Error; err != nil {
		return fail(err)
	}
	for _, r := range byFulfil {
		st.ByFulfillment[Fulfillment(r.Grp)] = r.N
		st.Lines += r.N
	}
	var evs []struct {
		EventID      uuid.UUID
		SellerOrgID  uuid.UUID
		Orders       int
		Lines        int
		LastPlacedAt time.Time
		Evidence     db.UUIDArray
		Total        int
	}
	if err := conn.Raw(`SELECT l.event_id, l.seller_org_id, count(DISTINCT l.order_id) AS orders, count(*) AS lines,
	    max(o.placed_at) AS last_placed_at,
	    coalesce(array_agg(l.fulfillment_evidence_file_id ORDER BY l.fulfillment_evidence_file_id)
	      FILTER (WHERE l.fulfillment_evidence_file_id IS NOT NULL), '{}') AS evidence,
	    count(*) OVER () AS total `+purchaseScope+`
	    AND l.event_id IS NOT NULL
	  GROUP BY l.event_id, l.seller_org_id
	  ORDER BY last_placed_at DESC, l.event_id, l.seller_org_id
	  LIMIT @limit`, args).Scan(&evs).Error; err != nil {
		return fail(err)
	}
	if len(evs) == 0 {
		return st, nil
	}
	ids := make([]uuid.UUID, len(evs))
	idx := make(map[[2]uuid.UUID]int, len(evs))
	for i, e := range evs {
		ids[i] = e.EventID
		idx[[2]uuid.UUID{e.EventID, e.SellerOrgID}] = i
		st.EventCount = e.Total
		st.Events = append(st.Events, EventPurchases{EventID: e.EventID, SellerOrgID: e.SellerOrgID, Orders: e.Orders,
			ByStatus: map[Status]int{}, Lines: e.Lines, ByFulfillment: map[Fulfillment]int{}, LastPlacedAt: e.LastPlacedAt,
			EvidenceFileIDs: []uuid.UUID(e.Evidence)})
	}
	args["events"] = ids
	var rows []struct {
		EventID     uuid.UUID
		SellerOrgID uuid.UUID
		Status      string
		Fulfillment string
		Orders      int
		Lines       int
	}
	if err := conn.Raw(`SELECT l.event_id, l.seller_org_id, o.status, '' AS fulfillment, count(DISTINCT o.id) AS orders, 0 AS lines `+
		purchaseScope+` AND l.event_id IN @events
	  GROUP BY l.event_id, l.seller_org_id, o.status
	  UNION ALL
	  SELECT l.event_id, l.seller_org_id, '', l.fulfillment_status, 0, count(*) `+purchaseScope+` AND l.event_id IN @events
	  GROUP BY l.event_id, l.seller_org_id, l.fulfillment_status`, args).Scan(&rows).Error; err != nil {
		return fail(err)
	}
	for _, r := range rows {
		i, ok := idx[[2]uuid.UUID{r.EventID, r.SellerOrgID}]
		if !ok {
			continue
		}
		if r.Status != "" {
			st.Events[i].ByStatus[Status(r.Status)] = r.Orders
		} else {
			st.Events[i].ByFulfillment[Fulfillment(r.Fulfillment)] = r.Lines
		}
	}
	return st, nil
}
