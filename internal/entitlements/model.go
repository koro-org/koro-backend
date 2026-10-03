package entitlements

import (
	"time"
	"uuid"
)

// Feature is entitlements.feature (02 §10.1): a platform feature a company
// pays for. Deliverables (stand, logo) are order lines, not entitlements
// (01 §2.1).
type Feature string

// Features.
const (
	FeatureTargetedPost     Feature = "targeted_post"     // quota: targeted posts (feed)
	FeatureTargetedDispatch Feature = "targeted_dispatch" // quota: notification dispatches (outreach)
	FeatureContactRequest   Feature = "contact_request"   // quota: contact requests (messaging)
	FeatureDiscoveryAccess  Feature = "discovery_access"  // access: level 2 discovery until valid_to
)

// Features lists every feature in 02 §10.1 order.
func Features() []Feature {
	return []Feature{FeatureTargetedPost, FeatureTargetedDispatch, FeatureContactRequest, FeatureDiscoveryAccess}
}

// Valid reports whether f is a known feature.
func (f Feature) Valid() bool {
	switch f {
	case FeatureTargetedPost, FeatureTargetedDispatch, FeatureContactRequest, FeatureDiscoveryAccess:
		return true
	}
	return false
}

// Kind returns the kind of the feature (entitlements_access_feature_chk:
// discovery_access is the only access right).
func (f Feature) Kind() Kind {
	if f == FeatureDiscoveryAccess {
		return KindAccess
	}
	return KindQuota
}

// RefType returns the usage ref type a consumption of f carries, or "" for
// an access right (nothing is consumed).
func (f Feature) RefType() RefType {
	switch f {
	case FeatureTargetedPost:
		return RefPost
	case FeatureTargetedDispatch:
		return RefDispatch
	case FeatureContactRequest:
		return RefContactRequest
	}
	return ""
}

// Kind is entitlements.kind.
type Kind string

// Kinds.
const (
	KindQuota  Kind = "quota"  // quantity_total units until valid_to
	KindAccess Kind = "access" // access between valid_from and valid_to
)

// Status is entitlements.status.
type Status string

// Statuses.
const (
	StatusPending   Status = "pending"   // granted by an unpaid order (BE-040); not usable
	StatusActive    Status = "active"    // usable inside [valid_from, valid_to)
	StatusExhausted Status = "exhausted" // quota fully used; a release makes it active again
	StatusExpired   Status = "expired"   // valid_to passed (entitlements.expire)
	StatusRevoked   Status = "revoked"   // taken back (admin, refund, unpaid order)
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusActive, StatusExhausted, StatusExpired, StatusRevoked:
		return true
	}
	return false
}

// SourceType is entitlements.source_type.
type SourceType string

// Source types.
const (
	SourceOrderLine  SourceType = "order_line"  // orders (BE-040): source_id = order_lines.id
	SourceAdminGrant SourceType = "admin_grant" // granted by hand (pilot packages sold offline, R-004d)
	SourcePromo      SourceType = "promo"       // promotion granted by hand
)

// Valid reports whether t is a known source type.
func (t SourceType) Valid() bool {
	switch t {
	case SourceOrderLine, SourceAdminGrant, SourcePromo:
		return true
	}
	return false
}

// RefType is entitlement_usages.ref_type: what consumed the unit.
type RefType string

// Ref types.
const (
	RefPost           RefType = "post"            // feed.posts.id (targeted_post)
	RefDispatch       RefType = "dispatch"        // outreach_dispatches.id (targeted_dispatch)
	RefContactRequest RefType = "contact_request" // contact_requests.id (contact_request)
)

// Valid reports whether t is a known ref type.
func (t RefType) Valid() bool {
	switch t {
	case RefPost, RefDispatch, RefContactRequest:
		return true
	}
	return false
}

// Ref identifies the record a consumption is for. (Type, ID) is unique
// among consumptions (entitlement_usages_ref_uq): consuming the same Ref
// again returns the first usage (idempotent).
type Ref struct {
	Type RefType
	ID   uuid.UUID
}

// Entitlement is one row of the entitlements table (02 §10.1).
type Entitlement struct {
	ID            uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	OrgID         uuid.UUID  `gorm:"type:uuid;column:org_id" json:"org_id"`
	Feature       Feature    `gorm:"column:feature" json:"feature"`
	Kind          Kind       `gorm:"column:kind" json:"kind"`
	QuantityTotal *int       `gorm:"column:quantity_total" json:"quantity_total,omitempty"`
	QuantityUsed  int        `gorm:"column:quantity_used" json:"quantity_used"`
	ValidFrom     time.Time  `gorm:"column:valid_from" json:"valid_from"`
	ValidTo       time.Time  `gorm:"column:valid_to" json:"valid_to"`
	Status        Status     `gorm:"column:status" json:"status"`
	SourceType    SourceType `gorm:"column:source_type" json:"source_type"`
	SourceID      *uuid.UUID `gorm:"type:uuid;column:source_id" json:"source_id,omitempty"`
	GrantedBy     *uuid.UUID `gorm:"type:uuid;column:granted_by" json:"granted_by,omitempty"`
	ActivatedAt   *time.Time `gorm:"column:activated_at" json:"activated_at,omitempty"`
	RevokedAt     *time.Time `gorm:"column:revoked_at" json:"revoked_at,omitempty"`
	RevokeReason  *string    `gorm:"column:revoke_reason" json:"revoke_reason,omitempty"`
	CreatedAt     time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

// TableName implements gorm's Tabler.
func (Entitlement) TableName() string { return "entitlements" }

// Remaining is the number of units left (0 for an access right).
func (e Entitlement) Remaining() int {
	if e.QuantityTotal == nil {
		return 0
	}
	return max(*e.QuantityTotal-e.QuantityUsed, 0)
}

// usableAt reports whether e can be consumed or grants access at now.
func (e Entitlement) usableAt(now time.Time) bool {
	return e.Status == StatusActive && !now.Before(e.ValidFrom) && now.Before(e.ValidTo)
}

// Usage is one row of entitlement_usages (02 §10.2; append-only). A
// consumption has a positive quantity; its reversal (Release) a negative
// one and ReversalOf set.
type Usage struct {
	ID            uuid.UUID  `gorm:"type:uuid;primaryKey"`
	EntitlementID uuid.UUID  `gorm:"type:uuid;column:entitlement_id"`
	OrgID         uuid.UUID  `gorm:"type:uuid;column:org_id"`
	Quantity      int        `gorm:"column:quantity"`
	RefType       RefType    `gorm:"column:ref_type"`
	RefID         uuid.UUID  `gorm:"type:uuid;column:ref_id"`
	ReversalOf    *uuid.UUID `gorm:"type:uuid;column:reversal_of"`
	ActorUserID   *uuid.UUID `gorm:"type:uuid;column:actor_user_id"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (Usage) TableName() string { return "entitlement_usages" }

// UsageView is a usage with the feature of its entitlement (listOrgEntitlementUsages).
type UsageView struct {
	Usage
	Feature Feature
}

// Availability is what an org may still use of a feature now (Available;
// 04 §2.11 "available"). Only active rows inside their validity window
// count.
type Availability struct {
	Feature Feature
	// Remaining is the sum of the units left (quota features; 0 for access).
	Remaining int
	// AccessUntil is the latest valid_to of a usable access right (nil for
	// quota features and when there is none).
	AccessUntil *time.Time
}

// Entitled reports whether the org may use the feature now: a unit left
// (quota) or a running access right.
func (a Availability) Entitled() bool {
	if a.Feature.Kind() == KindAccess {
		return a.AccessUntil != nil
	}
	return a.Remaining > 0
}

// GrantInput is a new entitlement (01 §4.4 Grant): an order line (pending,
// BE-040), an admin grant or a promotion.
type GrantInput struct {
	OrgID     uuid.UUID
	Feature   Feature
	Quantity  *int // quota features only: quantity_total ≥ 1
	ValidFrom time.Time
	ValidTo   time.Time
	// Status is pending (activated later by ActivateBySource) or active.
	Status     Status
	SourceType SourceType
	// SourceID is required except for admin_grant; (source_type, source_id,
	// feature) is unique: granting the same source and feature again
	// returns the first entitlement.
	SourceID *uuid.UUID
	// GrantedBy is the admin of an admin_grant (required there).
	GrantedBy *uuid.UUID
}
