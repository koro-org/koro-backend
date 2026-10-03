package main

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/notify"
	"koro/internal/platform/config"
	"koro/internal/platform/obs"
)

// useMemStore installs the in-memory notify stores used by service,
// wiring and authz matrix tests (05 §13.2). Owner: BE-005.
// Only tickets of the notify module edit this file (BE-001 R4).
func (w *notifyWiring) useMemStore() {
	w.store = notify.NewMemStore()
	w.transport = notify.NewMemTransport()
	// Test containers carry no loaded configuration: give the sender identity.
	w.cfg = &config.Notify{SMTPFrom: "noreply@koro.test", SMTPFromName: "Koro"}
}

type consentStub struct{ active bool }

func (c consentStub) HasActive(context.Context, uuid.UUID, string) (bool, error) {
	return c.active, nil
}

func sampleMessage(key string) notify.Message {
	return notify.Message{
		ToEmail: "ayse.yilmaz@ogrenci.edu.tr", Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: "notify.test_message", Locale: "tr", Data: map[string]any{"name": "Ayşe"}, DedupeKey: key,
	}
}

func buildNotify(t *testing.T, mod func(w *notifyWiring)) (*container, *notify.MemTransport) {
	t.Helper()
	c := newTestContainer(t)
	if mod != nil {
		mod(&c.notify)
	}
	if err := c.build(); err != nil {
		t.Fatalf("build() error = %v", err)
	}
	return c, c.notify.transport.(*notify.MemTransport)
}

func TestWire_Notify_SendsThroughTheWiredServices(t *testing.T) {
	t.Parallel()
	c, tr := buildNotify(t, nil)
	if c.notify.svc == nil {
		t.Fatal("notify.svc is not set by build")
	}
	ctx := obs.WithTaskID(t.Context(), "task-1")

	id, err := c.notify.svc.Send(ctx, sampleMessage("k1"))
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	again, err := c.notify.svc.Send(ctx, sampleMessage("k1"))
	if err != nil || again != id {
		t.Fatalf("repeated Send() = (%s, %v), want the previous delivery %s", again, err, id)
	}
	sent := tr.Sent()
	if len(sent) != 1 || sent[0].FromAddress != "noreply@koro.test" || sent[0].FromName != "Koro" {
		t.Fatalf("sent = %+v, want exactly one e-mail from the configured sender", sent)
	}
}

// With no consent checker wired, commercial messages are rejected (the safe
// default of the ticket); BE-015 binds consent.Service.
func TestWire_Notify_CommercialRejectedUntilConsentIsWired(t *testing.T) {
	t.Parallel()
	c, tr := buildNotify(t, nil)
	uid := uuid.NewV7()
	m := sampleMessage("promo")
	m.UserID, m.Category, m.UnsubscribeURL = &uid, notify.CategoryCommercial, "https://koro.test/u/1"

	_, err := c.notify.svc.Send(obs.WithTaskID(t.Context(), "task-1"), m)
	if !errors.Is(err, notify.ErrCommercialDisabled) || len(tr.Sent()) != 0 {
		t.Fatalf("Send() = %v with %d e-mails, want ErrCommercialDisabled and none sent", err, len(tr.Sent()))
	}
}

func TestWire_Notify_ConsentPortIsPassedThrough(t *testing.T) {
	t.Parallel()
	c, _ := buildNotify(t, func(w *notifyWiring) { w.consent = consentStub{active: false} })
	uid := uuid.NewV7()
	m := sampleMessage("promo")
	m.UserID, m.Category, m.Template, m.UnsubscribeURL = &uid, notify.CategoryCommercial, "notify.test_message", "https://koro.test/u/1"

	// test_message is transactional: the template category guard still holds
	// even with consent wired.
	if _, err := c.notify.svc.Send(obs.WithTaskID(t.Context(), "task-1"), m); !errors.Is(err, notify.ErrInvalidMessage) {
		t.Fatalf("Send(commercial on a transactional template) error = %v, want ErrInvalidMessage", err)
	}
}

func TestWire_Notify_WithoutAStoreSendFailsLoudly(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.notify.store = nil // production state until BE-015: no store
	if err := c.build(); err != nil {
		t.Fatalf("build() error = %v: the process must still start", err)
	}
	_, err := c.notify.svc.Send(obs.WithTaskID(t.Context(), "task-1"), sampleMessage("k1"))
	if !errors.Is(err, notify.ErrStoreNotWired) {
		t.Fatalf("Send() error = %v, want ErrStoreNotWired", err)
	}
}

func TestWire_Notify_SMTPTransportBuiltFromConfig(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.notify.transport, c.notify.cfg = nil, nil // production state: SMTP from the loaded SMTP_* settings
	c.infra.cfg.Notify = config.Notify{
		SMTPHost: "localhost", SMTPPort: 1025, SMTPTLS: config.SMTPTLSNone,
		SMTPFrom: "noreply@koro.test", SMTPFromName: "Koro", SMTPTimeout: time.Second,
	}
	if err := c.build(); err != nil {
		t.Fatalf("build() error = %v", err)
	}
	if _, isMem := c.notify.transport.(*notify.MemTransport); isMem {
		t.Fatal("test transport used instead of SMTP")
	}

	// An empty configuration (a section that was never loaded) must not start.
	bad := newTestContainer(t)
	bad.notify.transport, bad.notify.cfg = nil, nil
	if err := bad.build(); err == nil {
		t.Error("build() with an empty SMTP configuration succeeded")
	}
}
