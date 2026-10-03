package notify_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/notify"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

func TestSender_DedupeKey_ReturnsPreviousDelivery(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	first := mustSend(t, f, testMessage("ticket:r1:v1"))
	second := mustSend(t, f, testMessage("ticket:r1:v1"))
	if first != second {
		t.Fatalf("repeated Send returned %s, want the previous delivery %s", second, first)
	}
	if got := len(f.tr.Sent()); got != 1 {
		t.Fatalf("e-mails sent = %d, want exactly 1", got)
	}
	d := mustGet(t, f.store, first)
	if d.Status != notify.StatusSent || d.Attempts != 1 || d.Provider != "mem" || d.TemplateVersion != 1 {
		t.Fatalf("delivery = %+v, want sent after 1 attempt via mem, template version 1", d)
	}
	if d.SentAt == nil || !d.SentAt.Equal(t0) || d.ProviderMessageID == "" {
		t.Fatalf("delivery = %+v, want sent_at = %s and a provider message ID", d, t0)
	}

	other := mustSend(t, f, testMessage("ticket:r1:v2"))
	if other == first {
		t.Fatal("a different dedupe key reused the delivery")
	}
	if got := len(f.tr.Sent()); got != 2 {
		t.Fatalf("e-mails sent = %d, want 2", got)
	}
}

func TestSender_DedupeKey_FailedDeliveryIsRetriedOnTheSameRow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tr.FailWith(errors.New("dial tcp: connection refused"))

	id, err := f.svc.Send(workerCtx(t), testMessage("k1"))
	if !errors.Is(err, notify.ErrDeliveryFailed) || notify.IsPermanent(err) {
		t.Fatalf("Send() error = %v, want a transient ErrDeliveryFailed", err)
	}
	if d := mustGet(t, f.store, id); d.Status != notify.StatusFailed || d.Attempts != 1 || !strings.Contains(d.LastError, "connection refused") {
		t.Fatalf("delivery = %+v, want failed after 1 attempt with the error recorded", d)
	}

	f.tr.FailWith(nil)
	again, err := f.svc.Send(workerCtx(t), testMessage("k1"))
	if err != nil || again != id {
		t.Fatalf("retry = (%s, %v), want the same delivery %s", again, err, id)
	}
	d := mustGet(t, f.store, id)
	if d.Status != notify.StatusSent || d.Attempts != 2 || d.LastError != "" {
		t.Fatalf("delivery = %+v, want sent after 2 attempts with the error cleared", d)
	}
	if got := len(f.tr.Sent()); got != 1 {
		t.Fatalf("e-mails sent = %d, want 1", got)
	}
}

func TestSender_DedupeKey_AttemptInFlightIsNotSentTwice(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Another worker claimed the key and has not finished.
	held, state, err := f.store.Claim(t.Context(), notify.Delivery{
		ID: newID(), Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: "notify.test_message", TemplateVersion: 1, DedupeKey: "k1",
	}, f.clk.Now(), notify.DefaultLease)
	if err != nil || state != notify.ClaimAcquired {
		t.Fatalf("setup Claim = (%v, %v)", state, err)
	}

	id, err := f.svc.Send(workerCtx(t), testMessage("k1"))
	if !errors.Is(err, notify.ErrDeliveryInFlight) || notify.IsPermanent(err) || id != held.ID {
		t.Fatalf("Send() = (%s, %v), want the held delivery and a retryable ErrDeliveryInFlight", id, err)
	}
	if len(f.tr.Sent()) != 0 {
		t.Fatal("an in-flight delivery was sent by a second attempt")
	}

	// The holder crashed: after the lease the next attempt takes over.
	f.clk.Advance(notify.DefaultLease)
	id, err = f.svc.Send(workerCtx(t), testMessage("k1"))
	if err != nil || id != held.ID {
		t.Fatalf("Send() after lease = (%s, %v), want it to take over %s", id, err, held.ID)
	}
	if d := mustGet(t, f.store, id); d.Status != notify.StatusSent || d.Attempts != 2 {
		t.Fatalf("delivery = %+v, want sent on attempt 2", d)
	}
}

func TestSender_ConcurrentSameKey_SendsOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const n = 32
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { ids[i], errs[i] = f.svc.Send(workerCtx(t), testMessage("race")) })
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil && !errors.Is(err, notify.ErrDeliveryInFlight) {
			t.Fatalf("worker %d: error = %v, want nil or ErrDeliveryInFlight", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("worker %d got delivery %s, want %s", i, ids[i], ids[0])
		}
	}
	if got := len(f.tr.Sent()); got != 1 {
		t.Fatalf("e-mails sent = %d, want exactly 1", got)
	}
}

func TestSender_PermanentProviderRejection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tr.FailWith(notify.Permanent(errors.New("550 5.1.1 <ayse.yilmaz@ogrenci.edu.tr>: user unknown")))

	id, err := f.svc.Send(workerCtx(t), testMessage("k1"))
	if !errors.Is(err, notify.ErrDeliveryRejected) || !notify.IsPermanent(err) {
		t.Fatalf("Send() error = %v, want a permanent ErrDeliveryRejected", err)
	}
	if strings.Contains(err.Error(), "ayse.yilmaz@ogrenci.edu.tr") {
		t.Fatalf("returned error leaks the address: %v", err)
	}
	if d := mustGet(t, f.store, id); d.Status != notify.StatusFailed || strings.Contains(d.LastError, "ayse.yilmaz") {
		t.Fatalf("delivery = %+v, want failed with a scrubbed error", d)
	}
}

func TestSender_Commercial_WithoutConsentPort_Rejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t) // Deps.Consent == nil: the safe default

	msg := commercialMessage("promo:1")
	id, err := f.svc.Send(workerCtx(t), msg)
	if !errors.Is(err, notify.ErrCommercialDisabled) || !notify.IsPermanent(err) || id != uuid.Nil() {
		t.Fatalf("Send() = (%s, %v), want ErrCommercialDisabled and no delivery", id, err)
	}
	if len(f.tr.Sent()) != 0 {
		t.Fatal("a commercial message was sent without a consent checker")
	}
	// Not even an otherwise invalid commercial message gets further.
	msg.UnsubscribeURL = ""
	if _, err := f.svc.Send(workerCtx(t), msg); !errors.Is(err, notify.ErrCommercialDisabled) {
		t.Fatalf("Send(invalid commercial) error = %v, want ErrCommercialDisabled", err)
	}
	// Nothing was recorded: the key is still free.
	if _, state, err := f.store.Claim(t.Context(), notify.Delivery{ID: newID(), DedupeKey: "promo:1"}, f.clk.Now(), time.Minute); err != nil || state != notify.ClaimAcquired {
		t.Fatalf("Claim after rejection = (%v, %v), want a fresh acquisition", state, err)
	}
}

func commercialMessage(key string) notify.Message {
	m := testMessage(key)
	uid := newID()
	m.UserID = &uid
	m.Category = notify.CategoryCommercial
	m.Template = "notify.test_promo"
	m.UnsubscribeURL = "https://koro.test/unsubscribe/abc"
	return m
}

// promoFixture adds a commercial sample template next to the embedded ones.
func promoFixture(t *testing.T, consent notify.ConsentChecker) fixture {
	t.Helper()
	return newFixture(t, func(d *notify.Deps) {
		d.Consent = consent
		d.Templates = loadTemplates(t, withPromo(baseTemplates(t)))
	})
}

func TestSender_Commercial_ConsentGranted_SendsWithUnsubscribe(t *testing.T) {
	t.Parallel()
	consent := &fakeConsent{active: true}
	f := promoFixture(t, consent)

	id := mustSend(t, f, commercialMessage("promo:1"))
	if got := consent.asked(); len(got) != 1 || got[0] != notify.PurposeCommercialEmail {
		t.Fatalf("consent asked for %v, want [%s]", got, notify.PurposeCommercialEmail)
	}
	sent := f.tr.Sent()
	if len(sent) != 1 || sent[0].UnsubscribeURL != "https://koro.test/unsubscribe/abc" {
		t.Fatalf("sent = %+v, want one e-mail carrying the unsubscribe URL", sent)
	}
	if !strings.Contains(sent[0].HTML, "ticari elektronik iletidir") || !strings.Contains(sent[0].Text, "https://koro.test/unsubscribe/abc") {
		t.Fatalf("commercial footer missing:\nHTML: %s\nText: %s", sent[0].HTML, sent[0].Text)
	}
	if d := mustGet(t, f.store, id); d.Category != notify.CategoryCommercial || d.Status != notify.StatusSent {
		t.Fatalf("delivery = %+v, want a sent commercial delivery", d)
	}
}

func TestSender_Commercial_NoActiveConsent_Skipped(t *testing.T) {
	t.Parallel()
	consent := &fakeConsent{active: false}
	f := promoFixture(t, consent)

	id, err := f.svc.Send(workerCtx(t), commercialMessage("promo:1"))
	if err != nil {
		t.Fatalf("Send() error = %v, want nil: a skipped message is not a failure", err)
	}
	if d := mustGet(t, f.store, id); d.Status != notify.StatusSkippedNoConsent {
		t.Fatalf("status = %s, want skipped_no_consent", d.Status)
	}
	if len(f.tr.Sent()) != 0 {
		t.Fatal("a commercial message was sent without consent")
	}
	// The skip is final for this key: no new consent lookup, same delivery.
	again := mustSend(t, f, commercialMessage("promo:1"))
	if again != id || len(consent.asked()) != 1 {
		t.Fatalf("repeat = %s after %d consent lookups, want the same delivery after 1", again, len(consent.asked()))
	}
}

func TestSender_Commercial_ConsentLookupError_IsRetryable(t *testing.T) {
	t.Parallel()
	consent := &fakeConsent{err: errors.New("consent store down")}
	f := promoFixture(t, consent)

	id, err := f.svc.Send(workerCtx(t), commercialMessage("promo:1"))
	if !errors.Is(err, notify.ErrDeliveryFailed) || notify.IsPermanent(err) {
		t.Fatalf("Send() error = %v, want a transient ErrDeliveryFailed", err)
	}
	consent.mu.Lock()
	consent.err, consent.active = nil, true
	consent.mu.Unlock()
	if again := mustSend(t, f, commercialMessage("promo:1")); again != id || len(f.tr.Sent()) != 1 {
		t.Fatalf("retry = %s with %d e-mails, want the same delivery and 1 e-mail", again, len(f.tr.Sent()))
	}
}

func TestSender_Commercial_PushUsesPushPurpose(t *testing.T) {
	t.Parallel()
	consent := &fakeConsent{active: true}
	push := &recordingPush{}
	f := newFixture(t, func(d *notify.Deps) { d.Consent, d.Push = consent, push })

	uid := newID()
	id := mustSend(t, f, notify.Message{
		UserID: &uid, Channel: notify.ChannelPush, Category: notify.CategoryCommercial,
		Template: "feed.promo", DedupeKey: "p1",
	})
	if got := consent.asked(); len(got) != 1 || got[0] != notify.PurposeCommercialPush {
		t.Fatalf("consent asked for %v, want [%s]", got, notify.PurposeCommercialPush)
	}
	if d := mustGet(t, f.store, id); d.Provider != "recording" || d.ProviderMessageID != "push-1" || d.Status != notify.StatusSent {
		t.Fatalf("delivery = %+v, want sent through the push port", d)
	}
}

func TestSender_Transactional_NeverCarriesListUnsubscribe(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	m := testMessage("k1")
	m.UnsubscribeURL = "https://koro.test/unsubscribe/abc" // ignored for transactional mail
	mustSend(t, f, m)
	if got := f.tr.Sent()[0].UnsubscribeURL; got != "" {
		t.Fatalf("transactional e-mail carries UnsubscribeURL %q", got)
	}
}

func TestSender_RejectsRequestContext(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reqCtx := obs.WithRequestID(t.Context(), "req-1")

	if _, err := f.svc.Send(reqCtx, testMessage("k1")); !errors.Is(err, notify.ErrNotInWorker) || !notify.IsPermanent(err) {
		t.Fatalf("Send(request ctx) error = %v, want ErrNotInWorker", err)
	}
	// A worker may carry the originating request ID for correlation.
	if _, err := f.svc.Send(obs.WithTaskID(reqCtx, "task-1"), testMessage("k1")); err != nil {
		t.Fatalf("Send(worker ctx with request ID) error = %v", err)
	}
	// Tests, seeds and scripts have neither ID.
	if _, err := f.svc.Send(t.Context(), testMessage("k2")); err != nil {
		t.Fatalf("Send(plain ctx) error = %v", err)
	}
}

func TestSender_InvalidMessage(t *testing.T) {
	t.Parallel()
	uid := newID()
	nilID := uuid.Nil()
	tests := []struct {
		name     string
		mutate   func(m *notify.Message)
		wantLoc  string
		wantCode string
	}{
		{"unknown channel", func(m *notify.Message) { m.Channel = "sms" }, "message.channel", ""},
		{"unknown category", func(m *notify.Message) { m.Category = "marketing" }, "message.category", ""},
		{"empty dedupe key", func(m *notify.Message) { m.DedupeKey = "" }, "message.dedupe_key", ""},
		{"dedupe key too long", func(m *notify.Message) { m.DedupeKey = strings.Repeat("k", 201) }, "message.dedupe_key", ""},
		{"dedupe key with address", func(m *notify.Message) { m.DedupeKey = "invite:ayse@ogrenci.edu.tr" }, "message.dedupe_key", ""},
		{"dedupe key with newline", func(m *notify.Message) { m.DedupeKey = "a\nb" }, "message.dedupe_key", ""},
		{"missing address", func(m *notify.Message) { m.ToEmail = "" }, "message.to_email", ""},
		{"address with display name", func(m *notify.Message) { m.ToEmail = "Ayse <ayse@ogrenci.edu.tr>" }, "message.to_email", ""},
		{"address with header injection", func(m *notify.Message) { m.ToEmail = "a@b.com\r\nBcc: x@y.com" }, "message.to_email", ""},
		{"bad template id", func(m *notify.Message) { m.Template = "Notify/Test" }, "message.template", ""},
		{"unknown template", func(m *notify.Message) { m.Template = "notify.nope" }, "message.template", ""},
		{"nil user id", func(m *notify.Message) { m.UserID = &nilID }, "message.user_id", ""},
		{"push without user", func(m *notify.Message) { m.Channel = notify.ChannelPush }, "message.user_id", ""},
		{"in_app without user", func(m *notify.Message) { m.Channel = notify.ChannelInApp }, "message.user_id", ""},
		{"push with attachment", func(m *notify.Message) {
			m.Channel, m.UserID = notify.ChannelPush, &uid
			m.Attachments = []notify.Attachment{{Filename: "a.ics", ContentType: "text/calendar", Data: []byte("x")}}
		}, "message.attachments", ""},
		{"missing template data", func(m *notify.Message) { m.Data = map[string]any{} }, "message.data", ""},
		{"template is transactional, message commercial", func(m *notify.Message) {
			m.Category, m.UserID, m.UnsubscribeURL = notify.CategoryCommercial, &uid, "https://koro.test/u/1"
		}, "message.category", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, func(d *notify.Deps) { d.Consent = &fakeConsent{active: true} })
			m := testMessage("k1")
			tt.mutate(&m)

			id, err := f.svc.Send(workerCtx(t), m)
			if !errors.Is(err, notify.ErrInvalidMessage) || !notify.IsPermanent(err) || id != uuid.Nil() {
				t.Fatalf("Send() = (%s, %v), want ErrInvalidMessage and no delivery", id, err)
			}
			var pe *problem.Error
			if !errors.As(err, &pe) || !hasLocation(pe.Fields(), tt.wantLoc) {
				t.Fatalf("field errors = %+v, want one at %s", pe.Fields(), tt.wantLoc)
			}
			if len(f.tr.Sent()) != 0 {
				t.Fatal("an invalid message was sent")
			}
		})
	}
}

func hasLocation(fields []problem.FieldError, loc string) bool {
	for _, f := range fields {
		if f.Location == loc {
			return true
		}
	}
	return false
}

func TestSender_CommercialEmail_NeedsUnsubscribeURL(t *testing.T) {
	t.Parallel()
	f := promoFixture(t, &fakeConsent{active: true})
	for _, u := range []string{"", "http://koro.test/u/1", "mailto:u@koro.test", "https://", "https://koro.test/u 1", "https://koro.test/u>"} {
		m := commercialMessage("promo:" + u)
		m.UnsubscribeURL = u
		if _, err := f.svc.Send(workerCtx(t), m); !errors.Is(err, notify.ErrInvalidMessage) {
			t.Errorf("UnsubscribeURL %q: error = %v, want ErrInvalidMessage", u, err)
		}
	}
	m := commercialMessage("promo:inapp")
	m.Channel = notify.ChannelInApp
	if _, err := f.svc.Send(workerCtx(t), m); !errors.Is(err, notify.ErrInvalidMessage) {
		t.Errorf("commercial in_app: error = %v, want ErrInvalidMessage", err)
	}
	m = commercialMessage("promo:nouser")
	m.UserID = nil
	if _, err := f.svc.Send(workerCtx(t), m); !errors.Is(err, notify.ErrInvalidMessage) {
		t.Errorf("commercial without user: error = %v, want ErrInvalidMessage", err)
	}
}

func TestSender_RenderFailureLeavesNoDelivery(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	m := testMessage("k1")
	m.Data = map[string]any{} // the template reads Data.name

	if _, err := f.svc.Send(workerCtx(t), m); !errors.Is(err, notify.ErrInvalidMessage) {
		t.Fatalf("Send() error = %v, want ErrInvalidMessage", err)
	}
	// The key is free: a corrected message with the same key is delivered.
	mustSend(t, f, testMessage("k1"))
	if len(f.tr.Sent()) != 1 {
		t.Fatalf("e-mails sent = %d, want 1", len(f.tr.Sent()))
	}
}

type recordingPush struct {
	mu   sync.Mutex
	sent []notify.Message
}

func (p *recordingPush) Name() string { return "recording" }

func (p *recordingPush) SendPush(_ context.Context, m notify.Message) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, m)
	return fmt.Sprintf("push-%d", len(p.sent)), nil
}

func TestSender_PushAndInApp_NoopByDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	uid := newID()
	for _, ch := range []notify.Channel{notify.ChannelPush, notify.ChannelInApp} {
		id := mustSend(t, f, notify.Message{
			UserID: &uid, Channel: ch, Category: notify.CategoryTransactional,
			Template: "events.ticket_ready", DedupeKey: "k:" + string(ch),
		})
		d := mustGet(t, f.store, id)
		if d.Status != notify.StatusSent || d.Provider != "noop" || d.UserID == nil || *d.UserID != uid {
			t.Fatalf("%s delivery = %+v, want sent through the noop adapter for the user", ch, d)
		}
	}
	if len(f.tr.Sent()) != 0 {
		t.Fatal("push or in_app went out as e-mail")
	}
}

type fixedIndexer struct{}

func (fixedIndexer) Index(email string) []byte { return []byte("idx:" + email) }

func TestSender_RecipientIndexReceivesNormalisedAddressAndNothingElseIsStored(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(d *notify.Deps) { d.Recipients = fixedIndexer{} })
	m := testMessage("k1")
	m.ToEmail = "Ayse.Yilmaz@Ogrenci.edu.tr"
	m.Data = map[string]any{"name": "Gizli Ad"}
	id := mustSend(t, f, m)

	d := mustGet(t, f.store, id)
	if string(d.RecipientIndex) != "idx:ayse.yilmaz@ogrenci.edu.tr" {
		t.Fatalf("RecipientIndex = %q, want the index of the normalised address", d.RecipientIndex)
	}
	dump := fmt.Sprintf("%+v", d)
	for _, secret := range []string{"Ayse.Yilmaz@Ogrenci.edu.tr", "Gizli Ad"} {
		if strings.Contains(dump, secret) {
			t.Fatalf("delivery record holds %q: %s", secret, dump)
		}
	}
}

func TestSender_NoPIIInLogs(t *testing.T) {
	t.Parallel()
	const (
		addr  = "ayse.yilmaz@ogrenci.edu.tr"
		name  = "Ayşe Yılmaz"
		phone = "+90 532 123 45 67"
	)
	uid := newID()
	consent := &fakeConsent{active: false}
	// A plain slog handler: no obs ReplaceAttr/Scrub safety net, so what the
	// service itself puts into a log record is what is checked (05 §11.2: the
	// last-resort regexes are not a design).
	logs := &syncBuffer{}
	f := newFixture(t, func(d *notify.Deps) {
		d.Consent = consent
		d.Templates = loadTemplates(t, withPromo(baseTemplates(t)))
		d.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})

	m := testMessage("k-ok")
	m.UserID = &uid
	m.Data = map[string]any{"name": name, "phone": phone}
	mustSend(t, f, m)

	// A provider error that echoes the address and the phone number.
	f.tr.FailWith(errors.New("550 5.1.1 <" + addr + ">: user unknown, call " + phone))
	m.DedupeKey = "k-fail"
	_, failErr := f.svc.Send(workerCtx(t), m)
	f.tr.FailWith(nil)

	// A skipped commercial message.
	cm := commercialMessage("k-skip")
	cm.ToEmail, cm.Data = addr, map[string]any{"name": name}
	mustSend(t, f, cm)

	// Duplicate handling logs too.
	m.DedupeKey = "k-ok"
	mustSend(t, f, m)

	out := logs.String()
	if !strings.Contains(out, "notification handled") || !strings.Contains(out, "notification delivery failed") {
		t.Fatalf("expected log lines are missing:\n%s", out)
	}
	if !strings.Contains(out, uid.String()) {
		t.Fatal("logs must identify the user by ID")
	}
	for _, secret := range []string{addr, "ayse.yilmaz", name, "Yılmaz", "Ayşe", phone, "532 123", "Koro test iletisi"} {
		if strings.Contains(out, secret) {
			t.Fatalf("logs leak %q:\n%s", secret, out)
		}
		if failErr != nil && strings.Contains(failErr.Error(), secret) {
			t.Fatalf("returned error leaks %q: %v", secret, failErr)
		}
	}
	if !strings.Contains(out, `"recipient":"a***@o***.tr"`) {
		t.Fatalf("logs should carry the masked recipient a***@o***.tr:\n%s", out)
	}
}

// finishFailingStore loses the outcome write, like a database outage after the
// provider accepted the message.
type finishFailingStore struct{ notify.Store }

func (finishFailingStore) Finish(context.Context, uuid.UUID, notify.Result, time.Time) (notify.Delivery, error) {
	return notify.Delivery{}, errors.New("database unavailable")
}

func TestSender_OutcomeNotRecorded_StillReportsTheSend(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(d *notify.Deps) { d.Store = finishFailingStore{d.Store} })

	if _, err := f.svc.Send(workerCtx(t), testMessage("k1")); err != nil {
		t.Fatalf("Send() error = %v: the provider accepted the message, failing the task would send it twice", err)
	}
	if len(f.tr.Sent()) != 1 {
		t.Fatalf("e-mails sent = %d, want 1", len(f.tr.Sent()))
	}
	if !strings.Contains(f.logs.String(), "outcome not recorded") {
		t.Fatalf("the lost outcome must be logged:\n%s", f.logs.String())
	}
}

func TestSender_StoreNotWired_FailsLoudly(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(d *notify.Deps) { d.Store = notify.MissingStore{} })

	_, err := f.svc.Send(workerCtx(t), testMessage("k1"))
	if !errors.Is(err, notify.ErrStoreNotWired) || notify.IsPermanent(err) {
		t.Fatalf("Send() error = %v, want a transient ErrStoreNotWired", err)
	}
	if len(f.tr.Sent()) != 0 {
		t.Fatal("a message was sent without a delivery store")
	}
}

func TestNew_PanicsOnMissingDependencies(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	full := notify.Deps{
		Store: f.store, Transport: f.tr, Templates: tpl, Clock: f.clk,
		Log: obs.Discard(), FromAddress: "noreply@koro.test",
	}
	tests := map[string]func(d *notify.Deps){
		"Store":       func(d *notify.Deps) { d.Store = nil },
		"Transport":   func(d *notify.Deps) { d.Transport = nil },
		"Templates":   func(d *notify.Deps) { d.Templates = nil },
		"Clock":       func(d *notify.Deps) { d.Clock = nil },
		"Log":         func(d *notify.Deps) { d.Log = nil },
		"FromAddress": func(d *notify.Deps) { d.FromAddress = "not-an-address" },
	}
	for name, breakIt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := full
			breakIt(&d)
			defer func() {
				r := recover()
				err, _ := r.(error)
				if err == nil || !strings.Contains(err.Error(), "Deps."+name) {
					t.Fatalf("New() panic = %v, want a message naming Deps.%s", r, name)
				}
			}()
			notify.New(d)
		})
	}
	if notify.New(full).Service == nil {
		t.Fatal("New() with all dependencies returned no service")
	}
}
