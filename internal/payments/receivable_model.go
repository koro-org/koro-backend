package payments

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
)

// CollectionStatus is the state of a manual clawback collection.
type CollectionStatus string

const (
	CollectionRequested CollectionStatus = "requested"
	CollectionCollected CollectionStatus = "collected"
	CollectionRejected  CollectionStatus = "rejected"
)

// Valid reports whether the status is a collection state.
func (s CollectionStatus) Valid() bool {
	return s == CollectionRequested || s == CollectionCollected || s == CollectionRejected
}

// ReceivableCollection claims one whole incoming transfer for one paid-out
// order line. Its financial facts are derived from those immutable parents.
type ReceivableCollection struct {
	ID                  uuid.UUID `gorm:"type:uuid;primaryKey"`
	IncomingTransferID  uuid.UUID `gorm:"type:uuid"`
	OrderLineID         uuid.UUID `gorm:"type:uuid"`
	OrderID             uuid.UUID `gorm:"type:uuid"`
	PayeeID             uuid.UUID `gorm:"type:uuid"`
	OrgID               uuid.UUID `gorm:"type:uuid"`
	CollectionAccountID uuid.UUID `gorm:"type:uuid"`
	Amount              int64
	BankReference       string
	ReceiptFileID       uuid.UUID `gorm:"type:uuid"`
	Status              CollectionStatus
	RequestedBy         uuid.UUID  `gorm:"type:uuid"`
	ApprovedBy          *uuid.UUID `gorm:"type:uuid"`
	ApprovedAt          *time.Time
	RejectedBy          *uuid.UUID `gorm:"type:uuid"`
	RejectedAt          *time.Time
	RejectionReason     *string
	LedgerTransactionID *uuid.UUID `gorm:"type:uuid"`
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// TableName implements gorm Tabler.
func (ReceivableCollection) TableName() string { return "receivable_collections" }

// CollectionInput identifies existing bank evidence and the source of debt.
// Amount, account, payee, organization and receipt cannot be supplied here.
type CollectionInput struct{ IncomingTransferID, OrderLineID uuid.UUID }

// CollectionFilter narrows the finance queue.
type CollectionFilter struct {
	Status  CollectionStatus
	PayeeID *uuid.UUID
}

// CollectionQuery carries validated filters and keyset pagination.
type CollectionQuery struct {
	Filter  CollectionFilter
	After   *TimeCursor
	Limit   int
	invalid error
}

// CollectionPage is one page of financial evidence.
type CollectionPage struct {
	Items []ReceivableCollection
	Next  *TimeCursor
}

// CollectionStore joins the caller's transaction. Finance reads span orgs.
type CollectionStore interface {
	InsertCollection(context.Context, ReceivableCollection) error
	GetCollection(context.Context, uuid.UUID, bool) (ReceivableCollection, error)
	UpdateCollection(context.Context, ReceivableCollection) (ReceivableCollection, error)
	ListCollections(context.Context, CollectionFilter, *TimeCursor, int) ([]ReceivableCollection, error)
	PendingCollections(context.Context, uuid.UUID, uuid.UUID) (int64, error)
	CollectionsOfUser(context.Context, uuid.UUID) ([]ReceivableCollection, error)
}

// ReceivableService records an already received bank transfer with four eyes.
type ReceivableService interface {
	ListCollections(context.Context, authz.Actor, CollectionQuery) (CollectionPage, error)
	CreateCollection(context.Context, authz.Actor, CollectionInput) (ReceivableCollection, error)
	ApproveCollection(context.Context, authz.Actor, uuid.UUID, identity.ReauthProof) (ReceivableCollection, error)
	RejectCollection(context.Context, authz.Actor, uuid.UUID, string) (ReceivableCollection, error)
}
