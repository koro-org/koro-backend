package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/queue"
)

// alertKinds are the security notices that also go to the app (BE-205,
// Q-50): password, e-mail, two-step verification and phone changes.
var alertKinds = []string{
	identity.NoticePasswordChanged, identity.NoticeEmailChangeRequested, identity.NoticeEmailChanged,
	identity.NoticeMFAEnabled, identity.NoticeMFADisabled, identity.NoticeRecoveryCodeUsed,
	identity.NoticeRecoveryCodesRegenerated, identity.NoticeMFAReset,
	identity.NoticePhoneVerified, identity.NoticePhoneRemoved, identity.NoticeSMSMFAEnabled,
	identity.NoticeSMSMFADisabled, identity.NoticeSMSResetEnabled, identity.NoticeSMSResetDisabled,
}

// emailOnlyKinds are the notices an anonymous party can cause: they stay
// e-mail only.
var emailOnlyKinds = []string{identity.NoticeSignupAttempt, identity.NoticeSMSResetLimit}

// securityNoticeTask returns the identity.send_security_notice handler.
func securityNoticeTask(t *testing.T, e *env) queue.HandlerFunc {
	t.Helper()
	for _, task := range e.mod.Tasks() {
		if task.Name == identity.SendSecurityNoticeTaskName {
			return task.Handle
		}
	}
	t.Fatal("identity does not handle " + identity.SendSecurityNoticeTaskName)
	return nil
}

// runSecurityNotice runs one security notice of kind for u.
func runSecurityNotice(t *testing.T, e *env, u identity.User, kind string) (uuid.UUID, error) {
	t.Helper()
	ref := uuid.NewV7()
	payload, err := json.Marshal(map[string]any{"user_id": u.ID, "kind": kind, "ref": ref, "phone_hint": "**21"})
	if err != nil {
		t.Fatal(err)
	}
	return ref, securityNoticeTask(t, e)(workerCtx(t), payload)
}

// TestSecurityNotice_AlertsGoToAppAndPush: every alert kind sends, besides
// the e-mail, an in-app notification and a push of its own fixed template
// (identity.security_<kind>), linked to the security settings and with no
// data: no address, number, IP or device reaches them. Each channel has
// its own dedupe key; the e-mail keeps the key it had.
func TestSecurityNotice_AlertsGoToAppAndPush(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range alertKinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			u := e.activeUser(t, "alert@example.com")
			before := len(e.mail.sent())
			ref, err := runSecurityNotice(t, e, u, kind)
			if err != nil {
				t.Fatal(err)
			}
			msgs := e.mail.sent()[before:]
			if len(msgs) != 3 {
				t.Fatalf("sent %d messages, want in_app, push and e-mail: %+v", len(msgs), msgs)
			}
			key := "identity.security:" + ref.String()
			want := "identity.security_" + kind
			for i, ch := range []notify.Channel{notify.ChannelInApp, notify.ChannelPush} {
				m := msgs[i]
				switch {
				case m.Channel != ch || m.Template != want || m.Category != notify.CategoryTransactional:
					t.Fatalf("message %d = %s %s %s, want %s %s transactional", i, m.Channel, m.Template, m.Category, ch, want)
				case m.UserID == nil || *m.UserID != u.ID || m.Locale != u.Locale:
					t.Fatalf("%s: recipient %v locale %q, want %s %q", ch, m.UserID, m.Locale, u.ID, u.Locale)
				case m.LinkPath != notify.MySecurityLink() || m.LinkPath.String() != "/me/security":
					t.Fatalf("%s: link %q, want the security settings", ch, m.LinkPath)
				case m.ToEmail != "" || len(m.Data) != 0:
					t.Fatalf("%s carries personal data: to %q, data %v", ch, m.ToEmail, m.Data)
				case m.DedupeKey != key+":"+string(ch):
					t.Fatalf("%s: dedupe key %q, want %q", ch, m.DedupeKey, key+":"+string(ch))
				}
			}
			if m := msgs[2]; m.Channel != notify.ChannelEmail || m.ToEmail != u.Email || m.Data["kind"] != kind || m.DedupeKey != key {
				t.Fatalf("e-mail = %s to %q kind %v key %q", m.Channel, m.ToEmail, m.Data["kind"], m.DedupeKey)
			}

			meta, ok := tpl.Meta(want)
			if !ok {
				t.Fatalf("template %s is not embedded", want)
			}
			if meta.Preference != notify.PrefSecurity || meta.Priority != notify.PriorityHigh ||
				!slices.Equal(meta.Channels, []notify.Channel{notify.ChannelInApp, notify.ChannelPush}) {
				t.Fatalf("%s meta = %+v, want security, high, [in_app push]", want, meta)
			}
			for _, loc := range []string{"tr", "en"} {
				if n, ok := tpl.Notice(want, loc); !ok || n.Title == "" || n.Body == "" {
					t.Fatalf("%s %s notice = %+v", want, loc, n)
				}
			}
		})
	}
}

// TestSecurityNotice_AnonymousKindsStayEmailOnly: a notice anyone can
// cause (a signup with the address, SMS reset requests) never reaches the
// holder's phone.
func TestSecurityNotice_AnonymousKindsStayEmailOnly(t *testing.T) {
	t.Parallel()
	for _, kind := range emailOnlyKinds {
		e := newEnv(t)
		u := e.activeUser(t, "quiet-"+kind+"@example.com")
		before := len(e.mail.sent())
		if _, err := runSecurityNotice(t, e, u, kind); err != nil {
			t.Fatal(err)
		}
		msgs := e.mail.sent()[before:]
		if len(msgs) != 1 || msgs[0].Channel != notify.ChannelEmail {
			t.Fatalf("%s sent %+v, want the e-mail only", kind, msgs)
		}
	}
}

// TestSecurityNotice_PasswordChangeAlertsTheApp: a real password change
// queues the notice; its task sends the in-app notification and the push
// besides the e-mail.
func TestSecurityNotice_PasswordChangeAlertsTheApp(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.activeUser(t, "change@example.com")
	if err := e.svc.RequestPasswordReset(ctx, authz.Anonymous(), "change@example.com"); err != nil {
		t.Fatal(err)
	}
	tok := e.lastToken(t, "identity.password_reset")
	if err := e.svc.ConfirmPasswordReset(ctx, authz.Anonymous(), tok, "Yeni-Ve-Uzun-Parola-7302"); err != nil {
		t.Fatal(err)
	}
	before := len(e.mail.sent())
	e.runNotices(t)
	var got []notify.Channel
	for _, m := range e.mail.sent()[before:] {
		if m.Template == "identity.security_password_changed" || m.Template == "identity.security_notice" {
			got = append(got, m.Channel)
		}
	}
	if !slices.Equal(got, []notify.Channel{notify.ChannelInApp, notify.ChannelPush, notify.ChannelEmail}) {
		t.Fatalf("password change sent on %v, want in_app, push, email", got)
	}
}

// failingMailer passes every message to next until armed; then it
// refuses the channels in fail with their error and records every attempt.
type failingMailer struct {
	next  identity.Mailer
	mu    sync.Mutex
	armed bool
	fail  map[notify.Channel]error
	tried []notify.Channel
}

func (f *failingMailer) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed {
		return f.next.Send(ctx, m)
	}
	f.tried = append(f.tried, m.Channel)
	return uuid.NewV7(), f.fail[m.Channel]
}

// TestSecurityNotice_OneChannelDoesNotHoldBackOthers: every channel is
// tried whatever another one answers; a transient failure retries the task
// (notify's dedupe keys drop what already went), a permanent one alone
// ends it.
func TestSecurityNotice_OneChannelDoesNotHoldBackOthers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		fail          map[notify.Channel]error
		wantErr       bool
		wantPermanent bool
	}{
		{"e-mail refused", map[notify.Channel]error{notify.ChannelEmail: notify.ErrDeliveryRejected}, true, true},
		{"push fails for now", map[notify.Channel]error{notify.ChannelPush: errors.New("fcm unavailable")}, true, false},
		{"one refused, one transient", map[notify.Channel]error{notify.ChannelInApp: notify.ErrInvalidMessage,
			notify.ChannelEmail: notify.ErrDeliveryInFlight}, true, false},
		{"all delivered", nil, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fm := &failingMailer{fail: tt.fail}
			e := newEnv(t, func(d *identity.Deps) { fm.next, d.Mailer = d.Mailer, fm })
			u := e.activeUser(t, "fail@example.com")
			fm.mu.Lock()
			fm.armed = true
			fm.mu.Unlock()
			_, err := runSecurityNotice(t, e, u, identity.NoticeMFADisabled)
			if (err != nil) != tt.wantErr || queue.IsPermanent(err) != tt.wantPermanent {
				t.Fatalf("err = %v (permanent %v), want error %v permanent %v", err, queue.IsPermanent(err), tt.wantErr, tt.wantPermanent)
			}
			fm.mu.Lock()
			defer fm.mu.Unlock()
			if !slices.Equal(fm.tried, []notify.Channel{notify.ChannelInApp, notify.ChannelPush, notify.ChannelEmail}) {
				t.Fatalf("tried %v, want every channel", fm.tried)
			}
		})
	}
}
