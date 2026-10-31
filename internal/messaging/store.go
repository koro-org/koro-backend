package messaging

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence of messaging (02 §14): the GORM store over
// PostgreSQL and the in-memory store of the tests (05 §13.2) pass the same
// contract (store_contract_test.go, store_conversations_contract_test.go).
// Writes join the transaction carried by ctx.
//
// Store is split by aggregate into embedded interfaces (05 §2.2; BE-162): a
// service file reaches the store only through the narrow view of the
// aggregate it works on (the accessors below); only the KVKK provider
// (privacy.go), which covers every row of a data subject, holds the whole
// Store (store_views_test.go).
type Store interface {
	requestStore      // contact_requests
	blockStore        // contact_blocks
	conversationStore // conversations, conversation_messages, conversation_user_states (store_conversations.go)
	StatsStore        // the counts of a company's panel (stats.go, BE-163)
}

// requests is the contact_requests view of the store.
func (s *service) requests() requestStore { return s.d.Store }

// blocks is the contact_blocks view of the store.
func (s *service) blocks() blockStore { return s.d.Store }

// stats is the panel count view of the store (BE-163).
func (s *service) stats() StatsStore { return s.d.Store }

// conversations is the conversations view of the store.
func (s *service) conversations() conversationStore { return s.d.Store }

// requestStore is the contact_requests part of Store (02 §14.1).
type requestStore interface {
	// Insert writes a new request. A pending request of the same company
	// and student is ErrRequestPending (contact_requests_pending_uq).
	Insert(ctx context.Context, r ContactRequest) (ContactRequest, error)
	// Get returns the request id of the company orgID or ErrNotFound.
	Get(ctx context.Context, orgID, id uuid.UUID) (ContactRequest, error)
	// GetForStudent returns the request id addressed to studentID or
	// ErrNotFound.
	GetForStudent(ctx context.Context, studentID, id uuid.UUID) (ContactRequest, error)
	// Lock reads and row-locks the request id (a transition follows).
	Lock(ctx context.Context, id uuid.UUID) (ContactRequest, error)
	// Transition moves the request from status from to status to (the
	// answer time with it: accepted and declined carry respondedAt);
	// ok=false when the request is no longer in from.
	Transition(ctx context.Context, id uuid.UUID, from, to Status, respondedAt *time.Time) (ContactRequest, bool, error)
	// Pending returns the pending request of the company to the student
	// (ErrNotFound without one).
	Pending(ctx context.Context, orgID, studentID uuid.UUID) (ContactRequest, error)
	// DeclinedSince reports whether the student declined a request of the
	// company answered after since (the 90-day rule, 03 k19).
	DeclinedSince(ctx context.Context, orgID, studentID uuid.UUID, since time.Time) (bool, error)
	// ListByOrg returns a page of the company's requests, newest first.
	ListByOrg(ctx context.Context, orgID uuid.UUID, q ListQuery) ([]ContactRequest, error)
	// ListByStudent returns a page of the requests addressed to the
	// student, newest first.
	ListByStudent(ctx context.Context, studentID uuid.UUID, q ListQuery) ([]ContactRequest, error)
	// PendingOfCompany row-locks the pending requests of the company to
	// the student (a block declines them).
	PendingOfCompany(ctx context.Context, orgID, studentID uuid.UUID) ([]ContactRequest, error)
	// DueForExpiry row-locks up to limit pending requests whose expires_at
	// is at or before now, skipping rows another worker holds, oldest
	// expiry first.
	DueForExpiry(ctx context.Context, now time.Time, limit int) ([]ContactRequest, error)

	// ReceivedBy returns every request addressed to the user (KVKK export).
	ReceivedBy(ctx context.Context, studentID uuid.UUID) ([]ContactRequest, error)
	// SentBy returns every request the user sent for a company (KVKK
	// export; kept on erasure: the company's record).
	SentBy(ctx context.Context, senderID uuid.UUID) ([]ContactRequest, error)
	// DeleteReceived hard deletes every request addressed to the user
	// (account erasure) and returns how many went.
	DeleteReceived(ctx context.Context, studentID uuid.UUID) (int, error)

	// CountClosedBefore counts the closed requests whose closing moment
	// (updated_at) is before cutoff (contact_request_closed, dry run).
	CountClosedBefore(ctx context.Context, cutoff time.Time) (int, error)
	// PurgeClosed hard deletes up to limit of them, oldest first.
	PurgeClosed(ctx context.Context, cutoff time.Time, limit int) (int, error)
}

// blockStore is the contact_blocks part of Store (02 §14.2).
type blockStore interface {
	// InsertBlock records the student's block of the company; ok=false
	// when it already existed.
	InsertBlock(ctx context.Context, studentID, orgID uuid.UUID) (ok bool, err error)
	// DeleteBlock removes it; ok=false when there was none.
	DeleteBlock(ctx context.Context, studentID, orgID uuid.UUID) (ok bool, err error)
	// Blocked reports whether the student blocked the company.
	Blocked(ctx context.Context, studentID, orgID uuid.UUID) (bool, error)
	// Blocks returns a page of the student's blocks, newest first.
	Blocks(ctx context.Context, studentID uuid.UUID, after *BlockKey, limit int) ([]Block, error)
	// DeleteBlocks hard deletes the user's blocks (account erasure).
	DeleteBlocks(ctx context.Context, studentID uuid.UUID) (int, error)
}
