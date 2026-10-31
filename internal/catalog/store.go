package catalog

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence of catalog (05 §2): services, offerings and
// commerce policies. Two implementations pass the same contract
// (store_contract_test.go): the GORM store over PostgreSQL and the
// in-memory store of service, wiring and matrix tests (05 §13.2).
// Writes join the transaction carried by ctx (db.Conn).
type Store interface {
	StatsStore // the queue count of a university's panel (stats.go, BE-163)

	// InsertService writes a new service; services_code_uq → ErrServiceCodeTaken.
	InsertService(ctx context.Context, s ServiceItem) (ServiceItem, error)
	// GetService returns a service (any status) or ErrNotFound; lock takes
	// the row lock (FOR UPDATE) in the caller's transaction.
	GetService(ctx context.Context, id uuid.UUID, lock bool) (ServiceItem, error)
	// UpdateService writes the mutable columns of s (code and created_at
	// never change).
	UpdateService(ctx context.Context, s ServiceItem) (ServiceItem, error)
	// ListServices returns the services of the filter ordered by name, then id.
	ListServices(ctx context.Context, f ServiceFilter) ([]ServiceItem, error)
	// ServicesByID returns the services among ids that exist.
	ServicesByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ServiceItem, error)

	// InsertOffering writes a new draft offering.
	InsertOffering(ctx context.Context, o Offering) (Offering, error)
	// GetOffering returns an offering by ID (any org) or ErrNotFound.
	GetOffering(ctx context.Context, id uuid.UUID, lock bool) (Offering, error)
	// UpdateOffering writes the mutable columns of o when its status is
	// still from (targeted UPDATE, 06 H-56: never Save); ErrInvalidState when
	// the row moved on. quantity_sold is Reserve's and Release's only.
	UpdateOffering(ctx context.Context, o Offering, from OfferingStatus) (Offering, error)
	// ListOrgOfferings returns a page of the org's offerings (newest first by
	// (created_at, id)).
	ListOrgOfferings(ctx context.Context, orgID uuid.UUID, f OrgOfferingFilter, after *TimeKey, limit int) ([]Offering, error)
	// BrowseOfferings returns a page of the marketplace: active offerings
	// whose sales have not ended at f.Now.
	BrowseOfferings(ctx context.Context, f BrowseFilter, after *BrowseKey, limit int) ([]Offering, error)
	// ReviewQueue returns a page of the offerings that wait for the platform
	// decision (pending_approval, not approved yet), oldest submission first.
	ReviewQueue(ctx context.Context, after *TimeKey, limit int) ([]Offering, error)
	// UniversityQueue returns a page of the offerings that wait for a
	// university decision (pending_approval, requires_university_approval,
	// university_approval_status pending), oldest submission first; every
	// seller org (the service keeps one university's).
	UniversityQueue(ctx context.Context, after *TimeKey, limit int) ([]Offering, error)
	// LiveEventOfferings locks and returns the offerings of the org bound to
	// the event that are not cancelled (event.cancelled consumer).
	LiveEventOfferings(ctx context.Context, orgID, eventID uuid.UUID) ([]Offering, error)
	// Reserve adds qty to quantity_sold with one conditional UPDATE (02
	// §18.2): only an active offering inside its sales window at now with
	// enough stock; the last unit turns it sold_out. ok is false when the
	// condition did not hold (the service reads the row for the reason).
	Reserve(ctx context.Context, id uuid.UUID, qty int, now time.Time) (o Offering, ok bool, err error)
	// Release gives qty back (an expired or cancelled order); a sold_out
	// offering is active again. ok is false when fewer than qty were sold.
	Release(ctx context.Context, id uuid.UUID, qty int) (o Offering, ok bool, err error)

	// InsertPolicy writes a new policy; an overlapping period of the scope
	// is ErrPolicyOverlap (commerce_policies_no_overlap_uq).
	InsertPolicy(ctx context.Context, p CommercePolicy) (CommercePolicy, error)
	// GetPolicy returns a policy or ErrNotFound.
	GetPolicy(ctx context.Context, id uuid.UUID, lock bool) (CommercePolicy, error)
	// ClosePolicy sets valid_to of an open policy (the only change a row
	// takes, koro_policy_close_only); ErrInvalidState when it is closed.
	ClosePolicy(ctx context.Context, id uuid.UUID, validTo time.Time) (CommercePolicy, error)
	// ListPolicies returns the policies of the filter, newest valid_from first.
	ListPolicies(ctx context.Context, f PolicyFilter) ([]CommercePolicy, error)
	// PolicyAt returns the policy in effect at t for the first matching
	// scope in the order offering > service > seller_org > default (02 §18.3).
	PolicyAt(ctx context.Context, offeringID, serviceID, sellerOrgID uuid.UUID, t time.Time) (CommercePolicy, bool, error)
}

// ServiceFilter narrows ListServices.
type ServiceFilter struct {
	Category Category // "" = any
	// SellableOnly keeps the active, accountant-reviewed services (what the
	// organizations see, 04 §2.14).
	SellableOnly bool
}

// OrgOfferingFilter narrows ListOrgOfferings.
type OrgOfferingFilter struct {
	Status OfferingStatus // "" = any
}

// TimeKey is a keyset position (time, id) of a page.
type TimeKey struct {
	At time.Time
	ID uuid.UUID
}

// Browse sort keys (04 §2.14).
const (
	SortNewest   = "-created_at"       // default: created_at desc, id desc
	SortPrice    = "unit_price_amount" // price asc, id asc
	SortSalesEnd = "sales_end_at"      // sales end asc (open-ended last), id asc
)

// BrowseFilter narrows BrowseOfferings.
type BrowseFilter struct {
	Now      time.Time
	Q        string      // full-text query on the title (koro_tr), 2–100 characters
	OrgID    *uuid.UUID  // one seller
	OrgIDs   []uuid.UUID // sellers of a university (nil = any; empty = none)
	EventID  *uuid.UUID
	Category Category // the service's category
	PriceMax *int64
	Sort     string
}

// BrowseKey is the keyset position of a marketplace page: the sort value
// (created_at, unit_price_amount or sales_end_at; an open-ended sale sorts
// as farFuture) and the id.
type BrowseKey struct {
	At    time.Time
	Price int64
	ID    uuid.UUID
}

// farFuture is the sales_end_at sort value of an open-ended sale.
var farFuture = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// PolicyFilter narrows ListPolicies.
type PolicyFilter struct {
	Scope PolicyScope // "" = any
	At    *time.Time  // only the policies in effect at At
}
