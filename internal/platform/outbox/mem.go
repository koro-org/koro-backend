package outbox

import (
	"context"
	"slices"
	"sync"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
)

// Mem is the recording Writer of service tests (05 §8.2: "olay yazıldı mı"
// tests look at Events). It applies the same validation as the real writer
// (transaction for O topics, catalog, mechanism) and records an event only
// when the transaction commits (db.NopTx: when the outermost function
// returns nil).
type Mem struct {
	clock  clock.Clock
	mu     sync.Mutex
	events []domainevent.Envelope
}

// NewMem returns an empty recording Writer.
func NewMem(clk clock.Clock) *Mem { return &Mem{clock: clk} }

// Add implements Writer.
func (m *Mem) Add(ctx context.Context, e domainevent.Envelope) error {
	if !db.InTx(ctx) {
		return ErrNoTx
	}
	return m.record(ctx, e, domainevent.Outbox)
}

// AfterCommit implements Writer.
func (m *Mem) AfterCommit(ctx context.Context, e domainevent.Envelope) error {
	return m.record(ctx, e, domainevent.AfterCommit)
}

func (m *Mem) record(ctx context.Context, e domainevent.Envelope, mech domainevent.Mechanism) error {
	e, err := prepare(ctx, e, mech, m.clock)
	if err != nil {
		return err
	}
	db.AfterCommit(ctx, func(context.Context) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.events = append(m.events, e)
	})
	return nil
}

// Events returns the committed events in order.
func (m *Mem) Events() []domainevent.Envelope {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events)
}

// Topics returns the topics of the committed events in order.
func (m *Mem) Topics() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.events))
	for i, e := range m.events {
		out[i] = e.Topic
	}
	return out
}
