package orders

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/files"
)

// Statistics is the order read of the panels (stats, BE-163, BE-189; SR §8
// "sponsor talepleri", "etkinlik ve sponsorluk sonuçları"): counts only,
// never an amount, a buyer snapshot or a person. It is a system read
// without authorisation: the caller authorises its own read
// (stats.club.read, stats.company.read).
type Statistics interface {
	// SalesStats returns the orders placed in [from, to) that hold at
	// least one line of sellerOrgID.
	SalesStats(ctx context.Context, sellerOrgID uuid.UUID, from, to time.Time) (SalesStats, error)
	// PurchaseStats returns the orders buyerOrgID placed in [from, to),
	// their seller lines by fulfilment and the events those lines are
	// bound to (BE-189).
	PurchaseStats(ctx context.Context, buyerOrgID uuid.UUID, from, to time.Time) (PurchaseStats, error)
}

// SalesStats are a seller's orders of a range: every order counts once,
// by its status now.
type SalesStats struct {
	Orders   int
	ByStatus map[Status]int
}

// MaxPurchaseEvents bounds PurchaseStats.Events (a company's sponsored
// events of a year fit; EventCount says how many there are).
const MaxPurchaseEvents = 50

// PurchaseStats are a buyer's orders of a range (BE-189): every order
// counts once, by its status now. Lines and ByFulfillment count the lines
// of the sellers (a Koro feature right, without a payee, has no delivery)
// that did not drop out of their unpaid order (BE-134).
type PurchaseStats struct {
	Orders        int
	ByStatus      map[Status]int
	Lines         int
	ByFulfillment map[Fulfillment]int
	// Events are the events the lines are bound to, the latest placement
	// first, at most MaxPurchaseEvents; EventCount is all of them.
	Events     []EventPurchases
	EventCount int
}

// EventPurchases are the buyer's lines bound to one event of a range: the
// orders that hold them (each once, by its status now), the lines by
// fulfilment and their delivery evidence.
type EventPurchases struct {
	EventID       uuid.UUID
	SellerOrgID   uuid.UUID // = the event's org (06 H-55)
	Orders        int
	ByStatus      map[Status]int
	Lines         int
	ByFulfillment map[Fulfillment]int
	LastPlacedAt  time.Time // the latest placement of those orders
	// EvidenceFileIDs are the delivery evidence files of the lines (the
	// store's answer; at most one per line).
	EvidenceFileIDs []uuid.UUID
	// Evidence counts the lines whose evidence is attached and was not
	// rejected by the scan (clean or not scanned yet); EvidenceRejected
	// those whose evidence was rejected and waits for the seller's new
	// upload (BE-154, BE-158). The service fills both from the files'
	// states; the store leaves them 0.
	Evidence         int
	EvidenceRejected int
}

// StatsStore is the persistence of Statistics, part of Store.
type StatsStore interface {
	// SalesStats returns the seller's orders placed in [from, to); keyed by
	// the seller of the caller's path (order_lines_seller_idx).
	SalesStats(ctx context.Context, sellerOrgID uuid.UUID, from, to time.Time) (SalesStats, error)
	// PurchaseStats returns the buyer's orders placed in [from, to) with
	// at most limit events; keyed by the buyer of the caller's path
	// (orders_org_idx).
	PurchaseStats(ctx context.Context, buyerOrgID uuid.UUID, from, to time.Time, limit int) (PurchaseStats, error)
}

var _ Statistics = (*service)(nil)

// SalesStats implements Statistics.
func (s *service) SalesStats(ctx context.Context, sellerOrgID uuid.UUID, from, to time.Time) (SalesStats, error) {
	st, err := s.d.Store.SalesStats(ctx, sellerOrgID, from, to)
	if err != nil {
		return SalesStats{}, fmt.Errorf("orders: sales stats: %w", err)
	}
	return st, nil
}

// PurchaseStats implements Statistics: the store's counts with the states
// of the delivery evidence files (without the files module every attached
// evidence counts as attached).
func (s *service) PurchaseStats(ctx context.Context, buyerOrgID uuid.UUID, from, to time.Time) (PurchaseStats, error) {
	st, err := s.d.Store.PurchaseStats(ctx, buyerOrgID, from, to, MaxPurchaseEvents)
	if err != nil {
		return PurchaseStats{}, fmt.Errorf("orders: purchase stats: %w", err)
	}
	var ids []uuid.UUID
	for _, e := range st.Events {
		ids = append(ids, e.EvidenceFileIDs...)
	}
	var states map[uuid.UUID]files.State
	if len(ids) > 0 && s.d.Files != nil {
		if states, err = s.d.Files.States(ctx, ids); err != nil {
			return PurchaseStats{}, fmt.Errorf("orders: purchase stats: evidence states: %w", err)
		}
	}
	for i := range st.Events {
		e := &st.Events[i]
		e.Evidence, e.EvidenceRejected = 0, 0
		for _, id := range e.EvidenceFileIDs {
			if states[id] == files.StateRejected {
				e.EvidenceRejected++
			} else {
				e.Evidence++
			}
		}
	}
	return st, nil
}
