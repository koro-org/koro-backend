package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"gorm.io/gorm"
)

// TxManager draws transaction boundaries (05 §6.2). Services call it;
// handlers and stores never open transactions. Inside fn, stores reach the
// transaction through Conn(ctx, base).
//
// Rules: no external I/O inside fn (HTTP, SMTP, Redis, files); side effects
// are an outbox row or an AfterCommit hook. A sentinel returned by fn comes
// back unchanged.
type TxManager interface {
	// WithTx runs fn in a READ COMMITTED transaction: commit when fn returns
	// nil, rollback when it returns an error or panics (the panic keeps
	// unwinding). A nested call joins the outer transaction without a
	// SAVEPOINT (01 §0.4): an inner error rolls the whole transaction back
	// once it reaches the outermost WithTx. Commit-time errors (deferred
	// constraints) are returned through Translate; when ctx ended before
	// COMMIT the error wraps ctx.Err() and nothing is committed.
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
	// WithSerializableTx runs fn in a SERIALIZABLE transaction and retries
	// the whole fn on 40001/40P01 (4 attempts, jittered backoff): fn must be
	// idempotent. Nested inside a SERIALIZABLE transaction it joins it;
	// inside a READ COMMITTED one it fails with ErrNotSerializable.
	WithSerializableTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// ErrNotSerializable is returned by WithSerializableTx when the context
// already carries a weaker transaction (the isolation level of a running
// transaction cannot be raised).
var ErrNotSerializable = errors.New("db: WithSerializableTx called inside a non-serializable transaction")

// Serializable retry policy.
const (
	serializableAttempts = 4
	retryBaseDelay       = 10 * time.Millisecond
)

// txState is the transaction carried by a context. tx is nil under NopTx.
// A transaction belongs to one goroutine; so do its hooks.
type txState struct {
	tx           *gorm.DB
	serializable bool
	hooks        []func(context.Context)
}

type txKey struct{}

func stateOf(ctx context.Context) *txState {
	s, _ := ctx.Value(txKey{}).(*txState)
	return s
}

// Conn returns the transaction carried by ctx, or base when there is none,
// bound to ctx. Stores always use it instead of base directly.
func Conn(ctx context.Context, base *gorm.DB) *gorm.DB {
	if s := stateOf(ctx); s != nil && s.tx != nil {
		return s.tx.WithContext(ctx)
	}
	return base.WithContext(ctx)
}

// InTx reports whether ctx carries a transaction (NopTx included).
func InTx(ctx context.Context) bool { return stateOf(ctx) != nil }

// AfterCommit runs fn after the transaction carried by ctx commits; it never
// runs if the transaction rolls back. Without a transaction fn runs
// immediately. Hooks run in registration order, synchronously, with a
// context that keeps ctx's values but not its cancellation (01 §6.1 "A";
// queue.Enqueuer.AfterCommit builds on it).
func AfterCommit(ctx context.Context, fn func(context.Context)) {
	if s := stateOf(ctx); s != nil {
		s.hooks = append(s.hooks, fn)
		return
	}
	fn(ctx)
}

// NewTxManager returns the GORM-backed TxManager of orm.
func NewTxManager(orm *gorm.DB) TxManager { return &gormTx{orm: orm} }

type gormTx struct{ orm *gorm.DB }

func (m *gormTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if s := stateOf(ctx); s != nil && s.tx != nil {
		return fn(ctx) // join the outer transaction
	}
	return m.run(ctx, nil, fn)
}

func (m *gormTx) WithSerializableTx(ctx context.Context, fn func(context.Context) error) error {
	if s := stateOf(ctx); s != nil && s.tx != nil {
		if !s.serializable {
			return ErrNotSerializable
		}
		return fn(ctx)
	}
	opts := &sql.TxOptions{Isolation: sql.LevelSerializable}
	for attempt := 1; ; attempt++ {
		err := m.run(ctx, opts, fn)
		if attempt == serializableAttempts || !IsRetryable(err) {
			return err
		}
		if err := sleep(ctx, backoff(attempt)); err != nil {
			return err
		}
	}
}

// run executes one transaction attempt.
func (m *gormTx) run(ctx context.Context, opts *sql.TxOptions, fn func(context.Context) error) error {
	s := &txState{serializable: opts != nil}
	var tx *gorm.DB
	if opts != nil {
		tx = m.orm.WithContext(ctx).Begin(opts)
	} else {
		tx = m.orm.WithContext(ctx).Begin()
	}
	if tx.Error != nil {
		return fmt.Errorf("db: begin: %w", tx.Error)
	}
	s.tx = tx

	finished := false
	defer func() {
		if !finished { // fn failed or panicked
			tx.Rollback()
		}
	}()
	if err := fn(context.WithValue(ctx, txKey{}, s)); err != nil {
		return err
	}
	finished = true
	if err := tx.Commit().Error; err != nil {
		// Deferred constraints (DEFERRABLE INITIALLY DEFERRED, constraint
		// triggers) fail here; the server has already rolled back.
		if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
			// The request ended before COMMIT: database/sql rolled the
			// transaction back on its own and may report only
			// sql.ErrTxDone. Keep the context error in the chain so the
			// caller still sees the deadline (httpx: 503; T-315, BE-071).
			err = fmt.Errorf("%w (%w)", err, cerr)
		}
		return Translate(fmt.Errorf("db: commit: %w", err))
	}
	hookCtx := context.WithoutCancel(ctx)
	for _, h := range s.hooks {
		h(hookCtx)
	}
	return nil
}

func backoff(attempt int) time.Duration {
	d := retryBaseDelay << (attempt - 1)
	return d + rand.N(d) //nolint:gosec // retry jitter, not security relevant
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("db: retry: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

// NopTx is the TxManager of service tests with in-memory stores (05 §13.2).
// fn runs directly (a rollback is not emulated), nested calls join, and
// AfterCommit hooks run only when the outermost fn returns nil, as they
// would after a real commit.
type NopTx struct{}

// WithTx runs fn.
func (NopTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if InTx(ctx) {
		return fn(ctx)
	}
	s := &txState{serializable: true}
	if err := fn(context.WithValue(ctx, txKey{}, s)); err != nil {
		return err
	}
	hookCtx := context.WithoutCancel(ctx)
	for _, h := range s.hooks {
		h(hookCtx)
	}
	return nil
}

// WithSerializableTx runs fn (no retry is needed without a database).
func (n NopTx) WithSerializableTx(ctx context.Context, fn func(context.Context) error) error {
	return n.WithTx(ctx, fn)
}
