package notify_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/notify"
	"koro/internal/platform/problem"
)

func userActor(id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.KindUser, UserID: id, SessionID: uuid.NewV7(), StudentStatus: authz.StudentNone}
}

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func sampleFixture(t *testing.T) fixture {
	t.Helper()
	return newFixture(t, func(d *notify.Deps) { d.Templates = loadTemplates(t, withPromo(baseTemplates(t))) })
}

// auditRows returns every audit record the fixture's service wrote.
func auditRows(t *testing.T, f fixture) []audit.Record {
	t.Helper()
	rows, err := f.audit.List(t.Context(), audit.Filter{}, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestSend_SuppressedEmail_Skipped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.store.Suppress(t.Context(), notify.Suppression{EmailIndex: index("ayse.yilmaz@ogrenci.edu.tr"),
		Reason: notify.SuppressComplaint, Source: notify.SourceProviderWebhook, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	m := testMessage("k1")
	m.ToEmail = "  Ayse.Yilmaz@Ogrenci.edu.tr " // normalised before the lookup
	m.ToEmail = strings.TrimSpace(m.ToEmail)
	id, err := f.svc.Send(workerCtx(t), m)
	if err != nil {
		t.Fatalf("Send() error = %v, want nil: a suppressed address is skipped, not failed", err)
	}
	if d := mustGet(t, f.store, id); d.Status != notify.StatusSuppressed || d.Provider != "" {
		t.Fatalf("delivery = %+v, want suppressed", d)
	}
	if len(f.tr.Sent()) != 0 {
		t.Fatal("an e-mail went to a suppressed address")
	}
	// Final for the key; another address still gets mail.
	if again := mustSend(t, f, m); again != id {
		t.Fatalf("repeat = %s, want %s", again, id)
	}
	other := testMessage("k2")
	other.ToEmail = "mehmet@koro.test"
	mustSend(t, f, other)
	if len(f.tr.Sent()) != 1 {
		t.Fatalf("sent = %d, want 1", len(f.tr.Sent()))
	}
}

func TestSend_CommercialWithoutConsent_Skipped(t *testing.T) {
	t.Parallel()
	consent := &fakeConsent{active: false}
	f := promoFixture(t, consent)
	m := commercialMessage("promo:1")
	// A commercial message ignores preferences: switching every category
	// off neither blocks nor replaces the consent check.
	_ = f.store.UpsertPreferences(t.Context(), *m.UserID, []notify.Preference{{Category: notify.PrefEventUpdates, Channel: notify.ChannelEmail}}, t0)

	id, err := f.svc.Send(workerCtx(t), m)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if d := mustGet(t, f.store, id); d.Status != notify.StatusSkippedNoConsent {
		t.Fatalf("status = %s, want skipped_no_consent", d.Status)
	}
	if got := consent.asked(); len(got) != 1 || got[0] != notify.PurposeCommercialEmail {
		t.Fatalf("consent asked %v", got)
	}
	if len(f.tr.Sent()) != 0 {
		t.Fatal("a commercial e-mail went out without consent")
	}
	// The push channel asks for its own purpose.
	push := commercialMessage("promo:push")
	push.Channel, push.ToEmail, push.UnsubscribeURL, push.Data = notify.ChannelPush, "", "", nil
	if _, err := f.svc.Send(workerCtx(t), push); err != nil {
		t.Fatal(err)
	}
	if got := consent.asked(); got[len(got)-1] != notify.PurposeCommercialPush {
		t.Fatalf("push consent purpose = %v", got)
	}
}

func TestSend_TransactionalHonoursPreferences(t *testing.T) {
	t.Parallel()
	consent := &fakeConsent{active: false}
	f := newFixture(t, func(d *notify.Deps) {
		d.Templates = loadTemplates(t, withPromo(baseTemplates(t)))
		d.Consent = consent
	})
	uid := newID()
	if _, err := f.svc.PutMyPreferences(t.Context(), userActor(uid), []notify.Preference{
		{Category: notify.PrefEventUpdates, Channel: notify.ChannelEmail, Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	m := testMessage("ev:1")
	m.UserID, m.Template = &uid, "notify.test_notice"
	id, err := f.svc.Send(workerCtx(t), m)
	if err != nil {
		t.Fatal(err)
	}
	if d := mustGet(t, f.store, id); d.Status != notify.StatusSkippedPreference || len(f.tr.Sent()) != 0 {
		t.Fatalf("delivery = %+v, want skipped_preference and no e-mail", d)
	}
	// The same category on in_app is still on (default), and consent is
	// never asked for transactional messages.
	inApp := notify.Message{UserID: &uid, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
		Template: "notify.test_notice", DedupeKey: "ev:1:in_app"}
	if d := mustGet(t, f.store, mustSend(t, f, inApp)); d.Status != notify.StatusSent {
		t.Fatalf("in_app = %+v, want sent", d)
	}
	// security templates ignore the preferences entirely.
	sec := testMessage("sec:1")
	sec.UserID = &uid
	if d := mustGet(t, f.store, mustSend(t, f, sec)); d.Status != notify.StatusSent {
		t.Fatalf("security e-mail = %+v, want sent", d)
	}
	if len(consent.asked()) != 0 {
		t.Fatalf("consent asked for transactional messages: %v", consent.asked())
	}
}

func TestSend_InAppAndPushCarryNoPersonalData(t *testing.T) {
	t.Parallel()
	f := sampleFixture(t)
	uid := newID()
	for _, data := range []map[string]any{
		{"name": "Ayşe Yılmaz"},
		{"email": "ayse@koro.test"},
		{"Event": newID()},
		{"phone": 5551234567},
	} {
		_, err := f.svc.Send(workerCtx(t), notify.Message{UserID: &uid, Channel: notify.ChannelInApp,
			Category: notify.CategoryTransactional, Template: "notify.test_notice", Data: data, DedupeKey: fmt.Sprint(data)})
		if !errors.Is(err, notify.ErrInvalidMessage) {
			t.Errorf("Data %v: error = %v, want ErrInvalidMessage", data, err)
		}
	}
	// A Link comes from the catalog builders only; a builder fed a bad
	// parameter still cannot smuggle a foreign path (BE-122).
	for _, link := range []notify.Link{notify.ClubLink("a b"), notify.ClubLink("x/../../admin"), notify.ClubLink(""),
		notify.AdminOrderLink("KR1&next=//evil.example")} {
		_, err := f.svc.Send(workerCtx(t), notify.Message{UserID: &uid, Channel: notify.ChannelInApp,
			Category: notify.CategoryTransactional, Template: "notify.test_notice", LinkPath: link, DedupeKey: "l" + link.String()})
		if !errors.Is(err, notify.ErrInvalidMessage) {
			t.Errorf("LinkPath %q: error = %v, want ErrInvalidMessage", link, err)
		}
	}
	ev := newID()
	mustSend(t, f, notify.Message{UserID: &uid, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
		Template: "notify.test_notice", Data: map[string]any{"event_id": ev}, LinkPath: notify.MyRegistrationLink(ev), DedupeKey: "ok"})
	p, err := f.svc.ListMine(t.Context(), userActor(uid), notify.InAppQuery{})
	if err != nil || len(p.Items) != 1 {
		t.Fatalf("ListMine() = (%+v, %v)", p, err)
	}
	n := p.Items[0]
	if n.Title != "Yeni bir bildiriminiz var" || n.Data["event_id"] != ev.String() || n.LinkPath != "/me/registrations/"+ev.String() {
		t.Fatalf("notification = %+v, want the fixed text and the opaque ID", n)
	}
	// A template that reads Data in its in_app text does not load.
	bad := withPromo(baseTemplates(t))
	bad["notify/test_notice.tr.title.tmpl"].Data = []byte("Merhaba {{.Data.name}}")
	if _, err := notify.LoadTemplates(bad); err == nil || !strings.Contains(err.Error(), "fixed") {
		t.Fatalf("LoadTemplates(title with Data) error = %v, want a fixed-text error", err)
	}
}

func TestPreferences_SecurityCannotBeDisabled_422(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := userActor(newID())
	_, err := f.svc.PutMyPreferences(t.Context(), a, []notify.Preference{
		{Category: notify.PrefEventUpdates, Channel: notify.ChannelPush, Enabled: false},
		{Category: notify.PrefSecurity, Channel: notify.ChannelEmail, Enabled: false},
	})
	var pe *problem.Error
	if !errors.Is(err, notify.ErrSecurityLocked) || !errors.As(err, &pe) || pe.Status() != 422 {
		t.Fatalf("PutMyPreferences(security off) error = %v, want 422 notify.security_preference_locked", err)
	}
	// Nothing was stored (all or nothing).
	prefs, err := f.svc.MyPreferences(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefs) != 21 {
		t.Fatalf("MyPreferences() = %d pairs, want 21 (defaults included)", len(prefs))
	}
	for _, p := range prefs {
		want := notify.DefaultEnabled(p.Category, p.Channel)
		if p.Enabled != want {
			t.Fatalf("%s/%s = %v, want the default %v", p.Category, p.Channel, p.Enabled, want)
		}
	}
	// Switching security on is a no-op, other categories change.
	got, err := f.svc.PutMyPreferences(t.Context(), a, []notify.Preference{
		{Category: notify.PrefSecurity, Channel: notify.ChannelEmail, Enabled: true},
		{Category: notify.PrefEventUpdates, Channel: notify.ChannelPush, Enabled: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p.Category == notify.PrefEventUpdates && p.Channel == notify.ChannelPush && p.Enabled {
			t.Fatal("event_updates/push is still on")
		}
	}
	// Duplicates are 422 too; another user's preferences are untouched.
	if _, err := f.svc.PutMyPreferences(t.Context(), a, []notify.Preference{
		{Category: notify.PrefModeration, Channel: notify.ChannelEmail}, {Category: notify.PrefModeration, Channel: notify.ChannelEmail},
	}); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("duplicate pair error = %v", err)
	}
	if ok, _ := f.svc.Allowed(t.Context(), newID(), notify.PrefEventUpdates, notify.ChannelPush); !ok {
		t.Fatal("another user inherited the change")
	}
}

func TestMarkAllRead_OnlyOwn(t *testing.T) {
	t.Parallel()
	f := sampleFixture(t)
	me, other := newID(), newID()
	var mine uuid.UUID
	for i, u := range []uuid.UUID{me, me, other} {
		id := mustSend(t, f, notify.Message{UserID: &u, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
			Template: "notify.test_notice", DedupeKey: fmt.Sprintf("n%d", i)})
		if u == me {
			mine = id
		}
	}
	// Another user's notification ID is not found (IDOR, 03 §7.2).
	theirs, _ := f.store.ListInApp(t.Context(), other, false, nil, 10)
	if err := f.svc.MarkRead(t.Context(), userActor(me), theirs[0].ID); !errors.Is(err, notify.ErrNotificationNotFound) {
		t.Fatalf("MarkRead(other's) error = %v, want not found", err)
	}
	if err := f.svc.MarkRead(t.Context(), userActor(me), mine); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.MarkAllRead(t.Context(), userActor(me)); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.svc.ListMine(t.Context(), userActor(me), notify.InAppQuery{UnreadOnly: true}); len(p.Items) != 0 {
		t.Fatalf("my unread = %d, want 0", len(p.Items))
	}
	if p, _ := f.svc.ListMine(t.Context(), userActor(other), notify.InAppQuery{UnreadOnly: true}); len(p.Items) != 1 {
		t.Fatalf("the other user's unread = %d, want 1 (untouched)", len(p.Items))
	}
	// Paging: one per page, newest first.
	p1, _ := f.svc.ListMine(t.Context(), userActor(me), notify.InAppQuery{Limit: 1})
	if len(p1.Items) != 1 || p1.Next == nil {
		t.Fatalf("page 1 = %+v", p1)
	}
	p2, _ := f.svc.ListMine(t.Context(), userActor(me), notify.InAppQuery{Limit: 1, After: p1.Next})
	if len(p2.Items) != 1 || p2.Next != nil || p2.Items[0].ID == p1.Items[0].ID {
		t.Fatalf("page 2 = %+v", p2)
	}
	if err := f.svc.MarkAllRead(t.Context(), authz.Anonymous()); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("anonymous MarkAllRead error = %v", err)
	}
}

func TestPushToken_RegisterAndDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	me, other := userActor(newID()), userActor(newID())
	id, err := f.svc.RegisterPushToken(t.Context(), me, notify.PushTokenInput{Platform: notify.PushIOS, Token: "apns-token-1", AppVersion: "1.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	toks, _ := f.store.ActivePushTokens(t.Context(), me.UserID)
	if len(toks) != 1 || toks[0].ID != id || toks[0].SessionID == nil || *toks[0].SessionID != me.SessionID {
		t.Fatalf("tokens = %+v, want one bound to the session", toks)
	}
	if err := f.svc.DeletePushToken(t.Context(), other, id); !errors.Is(err, notify.ErrPushTokenNotFound) {
		t.Fatalf("DeletePushToken(other's) error = %v", err)
	}
	if _, err := f.svc.RegisterPushToken(t.Context(), me, notify.PushTokenInput{Platform: "symbian", Token: "x"}); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("bad platform error = %v", err)
	}
	if err := f.svc.DeletePushToken(t.Context(), me, id); err != nil {
		t.Fatal(err)
	}
	for _, r := range auditRows(t, f) {
		if strings.Contains(string(r.Metadata), "apns-token-1") {
			t.Fatal("the push token reached the audit log")
		}
	}
}

func TestAdminListUserDeliveries_NoContentOrData(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	uid := newID()
	m := testMessage("k1")
	m.UserID, m.Data = &uid, map[string]any{"name": "Gizli Ad Soyad"}
	id := mustSend(t, f, m)

	support := persona(t, "support")
	access := notify.Access{Reason: "Ticket 4711: user reports missing e-mails", TicketRef: "T-4711"}
	if _, err := f.svc.AdminListUserDeliveries(t.Context(), support, notify.Access{}, uid, notify.DeliveryQuery{}); !errors.Is(err, authz.ErrBreakGlassReason) {
		t.Fatalf("without a reason error = %v, want authz.break_glass_reason_required", err)
	}
	for _, name := range []string{"student", "dpo", "moderator", "finance_admin"} {
		if _, err := f.svc.AdminListUserDeliveries(t.Context(), persona(t, name), access, uid, notify.DeliveryQuery{}); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("%s error = %v, want forbidden", name, err)
		}
	}
	p, err := f.svc.AdminListUserDeliveries(t.Context(), support, access, uid, notify.DeliveryQuery{})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != id {
		t.Fatalf("AdminListUserDeliveries() = (%+v, %v)", p, err)
	}
	// The response DTO has exactly the 04 §2.19 fields: no content, data,
	// address, index or provider error.
	dto := notify.DeliveryDTO{}
	var fields []string
	for i := range reflect.TypeOf(dto).NumField() {
		fields = append(fields, reflect.TypeOf(dto).Field(i).Tag.Get("json"))
	}
	if want := []string{"delivery_id", "channel", "template", "status", "created_at"}; !reflect.DeepEqual(fields, want) {
		t.Fatalf("DeliveryDTO fields = %v, want %v", fields, want)
	}
	raw, _ := json.Marshal(notify.ToDeliveryDTO(p.Items[0]))
	for _, secret := range []string{"Gizli", "ayse", "ogrenci"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(secret)) {
			t.Fatalf("delivery DTO leaks %q: %s", secret, raw)
		}
	}
	// The read is audited as break-glass with the user as subject.
	rows := auditRows(t, f)
	if len(rows) != 1 || rows[0].Action != string(authz.NotifyDeliveryList) || rows[0].Reason != access.Reason {
		t.Fatalf("audit = %+v, want one notify.delivery.list record with the reason", rows)
	}
	var meta map[string]any
	_ = json.Unmarshal(rows[0].Metadata, &meta)
	if meta[audit.MetaBreakGlass] != true || meta[audit.MetaSubjectUserID] != uid.String() || meta[audit.MetaTicketRef] != "T-4711" {
		t.Fatalf("audit metadata = %v", meta)
	}
}

func TestAdminSuppressions_ListAndDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, e := range []string{"a@koro.test", "b@koro.test"} {
		if _, err := f.store.Suppress(t.Context(), notify.Suppression{EmailIndex: index(e), Reason: notify.SuppressHardBounce,
			Source: notify.SourceProviderWebhook, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	pad := persona(t, "platform_admin")
	access := notify.Access{Reason: "User fixed the mailbox, ticket 99"}
	if _, err := f.svc.AdminListSuppressions(t.Context(), pad, notify.Access{}, notify.SuppressionQuery{}); !errors.Is(err, authz.ErrBreakGlassReason) {
		t.Fatalf("platform_admin without a reason error = %v", err)
	}
	one, err := f.svc.AdminListSuppressions(t.Context(), pad, access, notify.SuppressionQuery{Email: "A@koro.test"})
	if err != nil || len(one.Items) != 1 || notify.SuppressionID(one.Items[0].EmailIndex) != notify.SuppressionID(index("a@koro.test")) {
		t.Fatalf("by address = (%+v, %v)", one, err)
	}
	all, _ := f.svc.AdminListSuppressions(t.Context(), pad, access, notify.SuppressionQuery{})
	if len(all.Items) != 2 {
		t.Fatalf("all = %d, want 2", len(all.Items))
	}
	id := notify.SuppressionID(index("a@koro.test"))
	if err := f.svc.AdminDeleteSuppression(t.Context(), pad, access, id); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{id, "zz", strings.ToUpper(id)} {
		if err := f.svc.AdminDeleteSuppression(t.Context(), pad, access, bad); !errors.Is(err, notify.ErrSuppressionNotFound) {
			t.Fatalf("delete %q error = %v, want not found", bad, err)
		}
	}
	for _, r := range auditRows(t, f) {
		if strings.Contains(string(r.Metadata), "koro.test") {
			t.Fatalf("the address reached the audit log: %s", r.Metadata)
		}
	}
}

func TestPrivacy_ExportAndErase(t *testing.T) {
	t.Parallel()
	f := sampleFixture(t)
	uid := newID()
	m := testMessage("k1")
	m.UserID = &uid
	mustSend(t, f, m)
	mustSend(t, f, notify.Message{UserID: &uid, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
		Template: "notify.test_notice", DedupeKey: "n1"})
	a := userActor(uid)
	if _, err := f.svc.RegisterPushToken(t.Context(), a, notify.PushTokenInput{Platform: notify.PushWeb, Token: "secret-device"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PutMyPreferences(t.Context(), a, []notify.Preference{{Category: notify.PrefFeedFollows, Channel: notify.ChannelPush}}); err != nil {
		t.Fatal(err)
	}
	sec, err := f.mod.Privacy.Export(t.Context(), uid)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, it := range sec.Items {
		kinds[it["kind"].(string)]++
	}
	if kinds["delivery"] != 2 || kinds["in_app_notification"] != 1 || kinds["push_token"] != 1 || kinds["preference"] != 1 {
		t.Fatalf("export kinds = %v", kinds)
	}
	raw, _ := json.Marshal(sec)
	if strings.Contains(string(raw), "secret-device") || strings.Contains(string(raw), "ayse") {
		t.Fatalf("export leaks the token or the address: %s", raw)
	}
	res, err := f.mod.Privacy.Erase(t.Context(), uid)
	if err != nil || res.Deleted != 5 {
		t.Fatalf("Erase() = (%+v, %v), want 5 rows deleted", res, err)
	}
	if res, _ := f.mod.Privacy.Erase(t.Context(), uid); res.Deleted != 0 {
		t.Fatal("Erase() is not idempotent")
	}
}
