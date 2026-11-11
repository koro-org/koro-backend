package notify_test

import (
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"uuid"

	"koro/internal/notify"
	"koro/internal/notify/pushtest"
	"koro/internal/platform/clock"
)

// TestTemplates_PushPriority (BE-177): a meta file declares the push
// priority, normal by default; only a security template may be high and
// the security alerts and conversation messages are.
func TestTemplates_PushPriority(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	high := map[string]bool{"identity.security_notice": true, "identity.mfa_notice": true, "identity.phone_notice": true,
		"identity.email_change": true, "identity.password_reset": true, "identity.role_alert": true,
		"messaging.conversation_message": true}
	// The in-app and push alerts of the security notices (BE-205).
	for _, kind := range []string{"password_changed", "email_change_requested", "email_changed", "mfa_enabled",
		"mfa_disabled", "recovery_code_used", "recovery_codes_regenerated", "mfa_reset", "phone_verified",
		"phone_removed", "sms_mfa_enabled", "sms_mfa_disabled", "sms_reset_enabled", "sms_reset_disabled",
		"new_sign_in"} { // BE-209: e-mail, in-app and push
		high["identity.security_"+kind] = true
	}
	for _, id := range tpl.IDs() {
		meta, _ := tpl.Meta(id)
		want := notify.PriorityNormal
		if high[id] {
			want = notify.PriorityHigh
		}
		if meta.Priority != want {
			t.Errorf("%s: priority %q, want %q", id, meta.Priority, want)
		}
	}

	for _, tt := range []struct{ name, meta, wantErr string }{
		{"unknown value", "version: 1\ncategory: transactional\npreference: security\npriority: urgent\n", "priority must be normal or high"},
		{"high outside security", "version: 1\ncategory: transactional\npreference: event_updates\npriority: high\n", "security templates and conversation messages only"},
		{"high on a commercial template", "version: 1\ncategory: commercial\npriority: high\n", "security templates and conversation messages only"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := baseTemplates(t)
			m["notify/test_message.meta.yaml"] = &fstest.MapFile{Data: []byte(tt.meta)}
			if _, err := notify.LoadTemplates(m); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadTemplates() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestSend_PushCarriesTemplatePriority: the push message carries the
// priority of its template (normal by default, high when declared).
func TestSend_PushCarriesTemplatePriority(t *testing.T) {
	t.Parallel()
	m := withPromo(baseTemplates(t))
	m["notify/test_alert.meta.yaml"] = &fstest.MapFile{Data: []byte("version: 1\ncategory: transactional\npreference: security\nchannels: [push]\npriority: high\n")}
	for _, loc := range []string{"tr", "en"} {
		m["notify/test_alert."+loc+".title.tmpl"] = &fstest.MapFile{Data: []byte("Hesap güvenliği")}
		m["notify/test_alert."+loc+".body.tmpl"] = &fstest.MapFile{Data: []byte("Hesabınızda bir değişiklik oldu.")}
	}
	rec := &recordingPush{}
	f := newFixture(t, func(d *notify.Deps) {
		d.Templates = loadTemplates(t, m)
		d.Push = rec
	})
	uid := newID()
	for i, tc := range []struct {
		template string
		want     notify.Priority
	}{{"notify.test_notice", notify.PriorityNormal}, {"notify.test_alert", notify.PriorityHigh}} {
		mustSend(t, f, notify.Message{UserID: &uid, Channel: notify.ChannelPush, Category: notify.CategoryTransactional,
			Template: tc.template, DedupeKey: "k" + tc.template})
		rec.mu.Lock()
		got := rec.sent[i].Priority
		rec.mu.Unlock()
		if got != tc.want {
			t.Fatalf("%s: push priority %q, want %q", tc.template, got, tc.want)
		}
	}
}

// TestPush_PriorityPerProvider (BE-177): the priority sets the web push
// Urgency only (high → high; normal and the zero value → normal); mobile
// keeps android.priority high on FCM and apns-priority 10 on APNs whatever
// the priority (the behaviour before BE-177).
func TestPush_PriorityPerProvider(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		p                   notify.Priority
		web, android, apple string
	}{
		{notify.PriorityHigh, "high", "high", "10"},
		{notify.PriorityNormal, "normal", "high", "10"},
		{"", "normal", "high", "10"},
	} {
		t.Run("priority "+string(tc.p), func(t *testing.T) {
			t.Parallel()
			user := uuid.NewV7()
			msg := pushMessage(user)
			msg.Priority = tc.p

			fcm := pushtest.NewFCM(t)
			e := newWebPushEnv(t, fcm)
			e.subscribe(t, user)
			devices(t, e.store, user, notify.PushAndroid, "android-token")
			if _, err := e.pt.SendPush(t.Context(), msg); err != nil {
				t.Fatal(err)
			}
			if got := e.wp.Messages(); len(got) != 1 || got[0].Priority != tc.web {
				t.Fatalf("web push = %+v, want Urgency %q", got, tc.web)
			}
			if got := fcm.Messages(); len(got) != 1 || got[0].Priority != tc.android {
				t.Fatalf("FCM = %+v, want android.priority %q", got, tc.android)
			}

			apns := pushtest.NewAPNs(t)
			clk := clock.NewFake(t0)
			store := notify.NewMemStore()
			devices(t, store, user, notify.PushIOS, strings.Repeat("ab", 32))
			a, err := notify.NewAPNs(notify.APNsConfig{KeyPEM: apns.KeyPEM, KeyID: apns.KeyID, TeamID: apns.TeamID, Topic: apns.Topic,
				BaseURL: apns.Server.URL, Clock: clk, HTTPClient: apns.Client()})
			if err != nil {
				t.Fatal(err)
			}
			pt, err := notify.NewPushTransport(notify.PushTransportConfig{Tokens: store, Sessions: testSessions, APNs: a, Clock: clk, Log: slog.New(slog.DiscardHandler)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pt.SendPush(t.Context(), msg); err != nil {
				t.Fatal(err)
			}
			if got := apns.Messages(); len(got) != 1 || got[0].Priority != tc.apple {
				t.Fatalf("APNs = %+v, want apns-priority %q", got, tc.apple)
			}
		})
	}
}
