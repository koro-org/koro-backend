package ledger

import (
	"context"
	"time"
	"uuid"
)

// PayoutStore is the persistence of payouts and payout_items (02 §21.4–5;
// BE-044). The GORM store (store_gorm_payout.go) and the in-memory store
// (store_mem_payout.go) pass the same contract test. Writes run in the
// transaction carried by ctx.
//
// The database is the last line (000113, 000115): the four-eyes CHECK, one
// CHECK per state (06 H-37), the account guard at creation and approval,
// the frozen columns, a line in one live payout only (payout_items_line_uq),
// items only for a fulfilled line of the payout's payee within its seller
// net, and at commit Σ items = amount for every live payout and every line
// of a paid payout marked paid out (payout_items_sum).
type PayoutStore interface {
	// InsertPayout writes a pending payout and its items.
	InsertPayout(ctx context.Context, p Payout, items []PayoutItem) (Payout, error)
	// GetPayout returns a payout, or ErrNotFound; lock takes its row lock
	// until the running transaction ends.
	GetPayout(ctx context.Context, id uuid.UUID, lock bool) (Payout, error)
	// PayoutItems returns the items of a payout in line order.
	PayoutItems(ctx context.Context, payoutID uuid.UUID) ([]PayoutItem, error)
	// TransitionPayout writes the state columns of next (status, approver,
	// payment, failure, cancellation) when the stored row is still in
	// state from; ErrInvalidState otherwise. The frozen columns are never
	// written (06 H-56).
	TransitionPayout(ctx context.Context, next Payout, from PayoutStatus) (Payout, error)
	// DeleteItems removes the items of a failed or cancelled payout (its
	// lines are payable again) and returns how many it removed.
	DeleteItems(ctx context.Context, payoutID uuid.UUID) (int, error)
	// ListPayouts returns up to limit payouts matching f, newest first
	// (created_at, then ID), strictly after the cursor.
	ListPayouts(ctx context.Context, f PayoutFilter, after *PayoutCursor, limit int) ([]Payout, error)
	// LinesInPayouts returns which of lineIDs are items of a live payout.
	LinesInPayouts(ctx context.Context, lineIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
	// PayoutAmounts returns the open and paid totals of a payee.
	PayoutAmounts(ctx context.Context, payeeID uuid.UUID) (PayoutAmounts, error)
	// PayeesOwed returns up to limit payees the ledger owes money to
	// (balance > 0), in ID order: the candidates of adminListPayables.
	PayeesOwed(ctx context.Context, limit int) ([]uuid.UUID, error)
	// HasOpenPayouts reports whether a seller org has a pending or approved
	// payout (closeOrg, hooks.openCommerce).
	HasOpenPayouts(ctx context.Context, orgID uuid.UUID) (bool, error)
	// PayoutsBy returns the payouts a user created, approved or marked paid
	// (KVKK export; financial records).
	PayoutsBy(ctx context.Context, userID uuid.UUID) ([]Payout, error)
}

// payoutStamp is the database's precision (microseconds).
func payoutStamp(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
