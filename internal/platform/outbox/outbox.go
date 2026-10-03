// Package outbox is the transactional outbox of Koro (05 §8.2–8.3, 01 §6;
// ADR-0009). Postgres and Redis are separate systems: a service never
// writes to both (dual write). Instead:
//
//   - Writer.Add (mechanism O) writes the event into the outbox table in the
//     business transaction; outside a transaction it fails with ErrNoTx.
//   - Writer.AfterCommit (mechanism A) enqueues one task per subscriber
//     after the commit, best effort.
//   - Relay (koro worker) reads due rows with FOR UPDATE SKIP LOCKED (safe
//     with several replicas), enqueues one asynq task per subscriber with
//     task ID "<outbox id>:<handler>" (a re-relayed row is deduplicated by
//     asynq), marks the row published, and on failure retries with
//     exponential back-off. A topic without subscribers is published at once.
//   - CleanupJob (outbox.cleanup, 01 §7) deletes published rows after the
//     retention period.
//
// Every topic belongs to domainevent's catalog and has a fixed mechanism:
// Add refuses an A topic and AfterCommit an O topic.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// Writer records domain events. Services depend on it.
type Writer interface {
	// Add writes e into the outbox in the transaction carried by ctx
	// (mechanism O). Without a transaction it returns ErrNoTx.
	Add(ctx context.Context, e domainevent.Envelope) error
	// AfterCommit enqueues one task per subscriber of e.Topic once the
	// transaction commits (mechanism A; at once without a transaction).
	// Enqueue failures are logged, not returned; the returned error reports
	// an invalid event only.
	AfterCommit(ctx context.Context, e domainevent.Envelope) error
}

// Errors.
var (
	// ErrNoTx: an outbox write outside db.WithTx is a programming error
	// (05 §8.2).
	ErrNoTx = errors.New("outbox: Add must run inside db.WithTx")
	// ErrWrongMechanism: the topic is delivered by the other mechanism
	// (domainevent catalog, 01 §6.2).
	ErrWrongMechanism = errors.New("outbox: topic uses the other delivery mechanism")
	// ErrInvalidEvent: a mandatory envelope field is missing.
	ErrInvalidEvent = errors.New("outbox: invalid event")
)

// Deps are the dependencies of Outbox.
type Deps struct {
	Store Store          // NewGormStore(db.ORM) in production
	Tx    db.TxManager   // the relay's transactions
	Subs  *Registry      // subscriptions of every module
	Queue queue.Enqueuer // asynq client
	Clock clock.Clock
	Log   *slog.Logger
}

// Outbox implements Writer and builds the relay and the cleanup job.
type Outbox struct{ d Deps }

// New returns the outbox.
func New(d Deps) *Outbox { return &Outbox{d: d} }

// Add implements Writer (mechanism O).
func (o *Outbox) Add(ctx context.Context, e domainevent.Envelope) error {
	if !db.InTx(ctx) {
		return ErrNoTx
	}
	e, err := prepare(ctx, e, domainevent.Outbox, o.d.Clock)
	if err != nil {
		return err
	}
	r := Row{
		ID: e.ID, Topic: e.Topic, Version: int16(e.Version), AggregateType: e.AggregateType, //nolint:gosec // version is a small schema counter (checked > 0)
		AggregateID: e.AggregateID, Payload: e.Payload, ActorUserID: e.ActorUserID,
		OccurredAt: e.OccurredAt, AvailableAt: e.OccurredAt,
	}
	if e.RequestID != "" {
		r.RequestID = &e.RequestID
	}
	return o.d.Store.Insert(ctx, r)
}

// AfterCommit implements Writer (mechanism A).
func (o *Outbox) AfterCommit(ctx context.Context, e domainevent.Envelope) error {
	e, err := prepare(ctx, e, domainevent.AfterCommit, o.d.Clock)
	if err != nil {
		return err
	}
	for _, s := range o.d.Subs.For(e.Topic) {
		o.d.Queue.AfterCommit(ctx, s.Name, e, TaskID(e.ID, s.Name), s.options()...)
	}
	return nil
}

// prepare validates e against the catalog and fills ID, OccurredAt, the
// actor and the request ID from ctx.
func prepare(ctx context.Context, e domainevent.Envelope, want domainevent.Mechanism, clk clock.Clock) (domainevent.Envelope, error) {
	s, ok := domainevent.Lookup(e.Topic)
	switch {
	case !ok:
		return e, fmt.Errorf("%w: %q", domainevent.ErrUnknownTopic, e.Topic)
	case s.Mechanism != want:
		return e, fmt.Errorf("%w: %s is %s (01 §6.2)", ErrWrongMechanism, e.Topic, s.Mechanism)
	case e.AggregateType == "" || e.AggregateID == uuid.Nil() || len(e.Payload) == 0:
		return e, fmt.Errorf("%w: %s needs aggregate type, aggregate ID and payload (use domainevent.New)", ErrInvalidEvent, e.Topic)
	}
	if e.Version == 0 {
		e.Version = s.Version
	}
	if e.ID == uuid.Nil() {
		e.ID = uuid.NewV7() // = outbox.id = consumer idempotency key
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = clk.Now()
	}
	e.OccurredAt = e.OccurredAt.UTC()
	if e.ActorUserID == nil {
		if id, err := uuid.Parse(obs.ActorID(ctx)); err == nil {
			e.ActorUserID = &id
		}
	}
	if e.RequestID == "" {
		e.RequestID = obs.RequestID(ctx)
	}
	return e, nil
}
