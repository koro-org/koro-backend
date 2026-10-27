package talent

import (
	"context"
)

// roleStore is the role of a talent transaction (01 §4.11, 02 §0.6): the
// switch to app_talent every other method requires and the way back to
// the session role (module.go talentTx, the audit rows, the retention
// dry run).
type roleStore interface {
	// Enter switches the transaction carried by ctx to app_talent and
	// returns the context the other methods require.
	Enter(ctx context.Context) (context.Context, error)
	// AsApp runs fn on the session role (app_rw) inside the same
	// transaction, then switches back to app_talent. fn writes public rows
	// only (audit); the talent tables are unreachable inside it.
	AsApp(ctx context.Context, fn func(context.Context) error) error
	// Leave switches the transaction back to the session role for good
	// (SET LOCAL ROLE NONE) and returns a context the talent methods
	// refuse. Only the retention dry run uses it: it runs inside the
	// transaction kvkk.retention_sweep opened (and always rolls back), and
	// hands that transaction back on the role it found.
	Leave(ctx context.Context) (context.Context, error)
}
