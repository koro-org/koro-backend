package consent_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/consent"
	"koro/internal/platform/iys"
	"koro/internal/platform/iys/iystest"
	"koro/internal/platform/iys/official"
	"koro/internal/platform/queue"
)

// recipients is the identity side of T-159: user → e-mail address.
type recipients struct {
	mu sync.Mutex
	m  map[uuid.UUID]string
}

func (r *recipients) set(id uuid.UUID, email string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if email == "" {
		delete(r.m, id)
		return
	}
	r.m[id] = email
}

func (r *recipients) Recipient(_ context.Context, id uuid.UUID, t iys.Type) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t != iys.Email || r.m[id] == "" {
		return "", consent.ErrIYSRecipientGone
	}
	return r.m[id], nil
}

func (r *recipients) User(_ context.Context, t iys.Type, address string) (uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, a := range r.m {
		if t == iys.Email && strings.EqualFold(a, address) {
			return id, nil
		}
	}
	return uuid.Nil(), consent.ErrIYSRecipientGone
}

type logSink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestIYS_SubmitAndReconcile runs consent's port over the official İYS
// adapter against the İYS double: a grant goes out as ONAY with the
// address resolved from identity, the Istanbul consent date and the İYS
// source of the channel; a withdrawal during an İYS outage fails and is
// delivered by consent.iys_reconcile as RET; an expired access token is
// renewed once; a record İYS refuses and an address that is gone (erased
// account) are permanent, alarmed and never leak the address.
func TestIYS_SubmitAndReconcile(t *testing.T) {
	t.Parallel()
	rcp := &recipients{m: map[uuid.UUID]string{}}
	var logs logSink
	var srv *iystest.Server
	e := newEnv(t, func(d *consent.Deps) {
		srv = iystest.New(t, iystest.WithClock(d.Clock))
		client, err := official.New(official.Config{
			BaseURL: srv.URL, Username: iystest.Username, Password: iystest.Password,
			IYSCode: iystest.IYSCode, BrandCode: iystest.BrandCode, Recipients: rcp, Clock: d.Clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		d.IYS = consent.NewIYSClient(client)
		d.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	})
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	ada, can := actor(t, "student").UserID, actor(t, "student_b").UserID
	rcp.set(ada, "ada.yilmaz@ogrenci.edu.tr")
	rcp.set(can, "can.demir@ogrenci.edu.tr")

	// 1. Grant over the mobile app → ONAY.
	grantID := e.record(t, grant(ada, text, consent.ChannelMobile))
	for _, err := range e.deliverAll(t) {
		if err != nil {
			t.Fatalf("delivery: %v", err)
		}
	}
	got := srv.Consents()
	want := iystest.Consent{ConsentDate: "2026-10-03 12:00:00", Source: "HS_MOBIL", Recipient: "ada.yilmaz@ogrenci.edu.tr",
		RecipientType: "BIREYSEL", Status: "ONAY", Type: "EPOSTA"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("İYS records = %+v, want [%+v]", got, want)
	}
	sub, _ := e.store.SubmissionOf(ctx, grantID)
	if sub.Status != consent.SubmissionSent || sub.IYSTransactionID == nil || len(*sub.IYSTransactionID) != 64 {
		t.Fatalf("granted submission = %+v", sub)
	}

	// 2. Withdrawal on the web while İYS is down → failed, retryable.
	srv.SetDown(true)
	withdrawID := e.record(t, withdraw(ada, consent.PurposeCommercialEmail))
	errs := e.deliverAll(t)
	if errs[len(errs)-1] == nil || queue.IsPermanent(errs[len(errs)-1]) {
		t.Fatalf("delivery during an outage = %v, want a retryable error", errs)
	}
	if sub, _ := e.store.SubmissionOf(ctx, withdrawID); sub.Status != consent.SubmissionFailed || sub.Attempts != 1 {
		t.Fatalf("withdrawal during an outage = %+v", sub)
	}

	// 3. İYS is back and the access token has expired: reconcile (after the
	// grace period) signs in again and sends RET once.
	srv.SetDown(false)
	srv.ExpireTokens()
	e.clk.Advance(consent.ReconcileGrace + time.Minute)
	reconcile := e.job(t, consent.ReconcileJobName)
	if err := reconcile.Handle(ctx); err != nil {
		t.Fatal(err)
	}
	got = srv.Consents()
	if len(got) != 2 || got[1].Status != "RET" || got[1].Source != "HS_WEB" || got[1].Recipient != "ada.yilmaz@ogrenci.edu.tr" {
		t.Fatalf("İYS records after reconcile = %+v", got)
	}
	if sub, _ := e.store.SubmissionOf(ctx, withdrawID); sub.Status != consent.SubmissionSent || sub.Attempts != 2 {
		t.Fatalf("withdrawal after reconcile = %+v", sub)
	}
	if n, r := srv.SignIns(), srv.Refreshes(); n != 1 || r != 1 {
		t.Fatalf("sign-ins = %d, refreshes = %d; want the first sign-in and one refresh after the 401", n, r)
	}

	// 4. İYS refuses Can's address: permanent, alarmed, address scrubbed.
	srv.Reject("can.demir@ogrenci.edu.tr")
	rejectedID := e.record(t, grant(can, text, consent.ChannelWeb))
	errs = e.deliverAll(t)
	if last := errs[len(errs)-1]; !queue.IsPermanent(last) || strings.Contains(last.Error(), "can.demir") {
		t.Fatalf("refused record = %v, want a permanent error without the address", last)
	}
	sub, _ = e.store.SubmissionOf(ctx, rejectedID)
	if sub.Status != consent.SubmissionFailed || sub.LastError == nil || !strings.Contains(*sub.LastError, "H464") ||
		strings.Contains(*sub.LastError, "can.demir") {
		t.Fatalf("refused submission = %+v", sub)
	}

	// 5. The account was erased before delivery (T-159): permanent + alarm.
	rcp.set(ada, "")
	goneID := e.record(t, grant(ada, text, consent.ChannelWeb))
	errs = e.deliverAll(t)
	if last := errs[len(errs)-1]; !queue.IsPermanent(last) || !errors.Is(last, queue.ErrPermanent) {
		t.Fatalf("erased recipient = %v, want permanent", last)
	}
	if sub, _ := e.store.SubmissionOf(ctx, goneID); sub.Status != consent.SubmissionFailed {
		t.Fatalf("erased recipient submission = %+v", sub)
	}
	out := logs.String()
	if strings.Count(out, `"alarm":true`) < 2 {
		t.Fatalf("permanent failures did not raise the alarm: %s", out)
	}
	for _, secret := range []string{"ada.yilmaz", "can.demir", iystest.Password, "iys-access-"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains %q: %s", secret, out)
		}
	}

	// 6. Sent submissions are never sent again.
	e.clk.Advance(24 * time.Hour)
	if err := reconcile.Handle(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Consents()); n != 2 {
		t.Fatalf("İYS has %d records after the daily reconcile, want 2", n)
	}
}
