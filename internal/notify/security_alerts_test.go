package notify_test

import (
	"slices"
	"strings"
	"testing"

	"koro/internal/notify"
)

// TestSend_SecurityAlertsReachInboxAndPush (BE-205): every in-app and push
// alert of identity's security notices (identity.security_<kind>) goes out
// with the security settings link, at high push priority, to every push
// surface: the security category cannot be switched off on any channel
// (02 §6.5 CHECK; the store refuses the row, Allowed never asks).
func TestSend_SecurityAlertsReachInboxAndPush(t *testing.T) {
	t.Parallel()
	rec := &recordingPush{}
	f := newFixture(t, func(d *notify.Deps) { d.Push = rec })
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	var alerts []string
	for _, id := range tpl.IDs() {
		if strings.HasPrefix(id, "identity.security_") && id != "identity.security_notice" {
			alerts = append(alerts, id)
		}
	}
	if len(alerts) != 14 {
		t.Fatalf("found %d security alert templates, want 14: %v", len(alerts), alerts)
	}
	uid := newID()
	for _, ch := range notify.PrefChannels() {
		if ok, err := f.svc.Allowed(t.Context(), uid, notify.PrefSecurity, ch); err != nil || !ok {
			t.Fatalf("Allowed(security, %s) = %v, %v; want always allowed", ch, ok, err)
		}
	}
	link := notify.MySecurityLink()
	for _, id := range alerts {
		for _, ch := range []notify.Channel{notify.ChannelInApp, notify.ChannelPush} {
			d := mustGet(t, f.store, mustSend(t, f, notify.Message{UserID: &uid, Channel: ch, Category: notify.CategoryTransactional,
				Template: id, Locale: "en", LinkPath: link, DedupeKey: id + ":" + string(ch)}))
			if d.Status != notify.StatusSent {
				t.Fatalf("%s %s: delivery %+v, want sent", id, ch, d)
			}
		}
	}

	rows, err := f.store.ListInApp(t.Context(), uid, false, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(alerts) {
		t.Fatalf("inbox has %d rows, want %d", len(rows), len(alerts))
	}
	for _, r := range rows {
		want, _ := tpl.Notice(r.Kind, "en")
		if r.LinkPath != "/me/security" || r.Title != want.Title || r.Body != want.Body || len(r.Data) != 0 {
			t.Fatalf("inbox row %+v, want the fixed English text, the security link and no data", r)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sent) != len(alerts) {
		t.Fatalf("pushed %d, want %d", len(rec.sent), len(alerts))
	}
	for _, p := range rec.sent {
		if p.Priority != notify.PriorityHigh || p.LinkPath != "/me/security" || len(p.Data) != 0 ||
			(p.Channels != nil && !slices.Equal(p.Channels, []notify.Channel{notify.ChannelPush, notify.ChannelWebPush})) {
			t.Fatalf("push %+v, want high priority, the security link, no data, every surface", p)
		}
	}
}
