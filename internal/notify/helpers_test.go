package notify_test

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/obs"
)

var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeConsent answers HasActive from a fixed result and records the purposes asked.
type fakeConsent struct {
	mu       sync.Mutex
	active   bool
	err      error
	purposes []string
}

func (c *fakeConsent) HasActive(_ context.Context, _ uuid.UUID, purpose string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purposes = append(c.purposes, purpose)
	return c.active, c.err
}

func (c *fakeConsent) asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.purposes...)
}

type fixture struct {
	svc   notify.Service
	store notify.Store
	tr    *notify.MemTransport
	clk   *clock.Fake
	logs  *syncBuffer
}

// newFixture wires the real service with the in-memory store, the in-memory
// transport, the embedded templates and a real masking logger. mod adjusts
// Deps before New.
func newFixture(t *testing.T, mod ...func(*notify.Deps)) fixture {
	t.Helper()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatalf("EmbeddedTemplates() error = %v", err)
	}
	f := fixture{
		store: notify.NewMemStore(),
		tr:    notify.NewMemTransport(),
		clk:   clock.NewFake(t0),
		logs:  &syncBuffer{},
	}
	d := notify.Deps{
		Store:       f.store,
		Transport:   f.tr,
		Templates:   tpl,
		Clock:       f.clk,
		Log:         obs.NewLogger(f.logs, obs.LoggerOptions{Level: slog.LevelDebug, Format: obs.FormatJSON}),
		FromAddress: "noreply@koro.test",
		FromName:    "Koro",
	}
	for _, m := range mod {
		m(&d)
	}
	f.svc = notify.New(d).Service
	return f
}

// workerCtx is a context as the worker builds it: it carries a task ID.
func workerCtx(t *testing.T) context.Context {
	t.Helper()
	return obs.WithTaskID(t.Context(), "task-1")
}

// testMessage is a valid transactional e-mail using the embedded sample template.
func testMessage(key string) notify.Message {
	return notify.Message{
		ToEmail:   "ayse.yilmaz@ogrenci.edu.tr",
		Channel:   notify.ChannelEmail,
		Category:  notify.CategoryTransactional,
		Template:  "notify.test_message",
		Locale:    "tr",
		Data:      map[string]any{"name": "Ayşe Yılmaz"},
		DedupeKey: key,
	}
}

func newID() uuid.UUID { return uuid.NewV7() }

func mustGet(t *testing.T, s notify.Store, id uuid.UUID) notify.Delivery {
	t.Helper()
	d, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return d
}

func mustSend(t *testing.T, f fixture, m notify.Message) uuid.UUID {
	t.Helper()
	id, err := f.svc.Send(workerCtx(t), m)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	return id
}
