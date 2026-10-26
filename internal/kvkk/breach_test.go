package kvkk_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/kvkk"
	"koro/internal/notify"
	"koro/internal/platform/civil"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

func (e *env) newBreach(t *testing.T, detected time.Time) kvkk.Breach {
	t.Helper()
	n := 40
	b, err := e.svc.CreateBreach(t.Context(), actor(t, "dpo"), kvkk.BreachInput{Title: "Açık alıcı listesi",
		DetectedAt: detected, DataCategories: []string{"contact", "identity", "contact"}, AffectedEstimate: &n,
		Description: "Bir duyuru alıcıları gizlemeden gönderildi.", OwnerUserID: authztest.ID("dpo")})
	if err != nil {
		t.Fatalf("CreateBreach: %v", err)
	}
	return b
}

// TestBreach_DeadlineIs72h (KVKK md. 12/5; 02 §16.4): the Board deadline is
// detected_at + 72 hours, the controller notice detected_at + 24 hours;
// breach.recorded alerts the DPO mailbox and every platform admin and the
// owner in-app, without the breach's content.
func TestBreach_DeadlineIs72h(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	detected := e.clk.Now().Add(-5 * time.Hour)
	b := e.newBreach(t, detected)
	if !b.AuthorityDeadlineAt.Equal(detected.Add(72*time.Hour)) || !b.ControllerDeadlineAt().Equal(detected.Add(24*time.Hour)) {
		t.Fatalf("deadlines = %s / %s", b.AuthorityDeadlineAt, b.ControllerDeadlineAt())
	}
	if b.Status != kvkk.BreachOpen || !slices.Equal(b.DataCategories, []string{"contact", "identity"}) {
		t.Fatalf("breach = %+v", b)
	}
	if got := e.topics(); !slices.Contains(got, domainevent.TopicBreachRecorded) {
		t.Fatalf("topics = %v", got)
	}
	if err := e.deliverTopic(t, domainevent.TopicBreachRecorded); err != nil {
		t.Fatal(err)
	}
	msgs := e.mail.of("kvkk.breach_recorded")
	var mail *notify.Message
	var inApp []uuid.UUID
	for i, m := range msgs {
		switch m.Channel {
		case notify.ChannelEmail:
			mail = &msgs[i]
		case notify.ChannelInApp:
			inApp = append(inApp, *m.UserID)
			if len(m.Data) != 1 || m.Data["breach_id"] != b.ID.String() {
				t.Errorf("in-app data = %v; opaque IDs only", m.Data)
			}
		}
	}
	if mail == nil || mail.ToEmail != dpoMailbox || mail.Data["authority_deadline"] == "" {
		t.Fatalf("DPO mail = %+v", mail)
	}
	for _, v := range mail.Data {
		if s, ok := v.(string); ok && (strings.Contains(s, "alıcı") || strings.Contains(s, "duyuru")) {
			t.Fatalf("breach content in the e-mail: %v", mail.Data)
		}
	}
	slices.SortFunc(inApp, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	want := []uuid.UUID{authztest.ID("platform_admin"), authztest.ID("dpo")}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(inApp, want) {
		t.Fatalf("in-app recipients = %v, want the platform admins and the owner", inApp)
	}
	if got := e.audit.of(string(authz.KvkkBreachManage)); len(got) != 1 {
		t.Fatalf("audit = %+v", got)
	}

	// Rules of the register.
	ctx, dpo := t.Context(), actor(t, "dpo")
	_, err := e.svc.CreateBreach(ctx, dpo, kvkk.BreachInput{Title: "x", DetectedAt: e.clk.Now().Add(time.Hour),
		Description: "y", OwnerUserID: authztest.ID("dpo")})
	problemtest.WantErr(t, err, kvkk.ErrInvalidRequest) // detection in the future
	_, err = e.svc.CreateBreach(ctx, dpo, kvkk.BreachInput{Title: "x", DetectedAt: e.clk.Now(), Description: "y",
		OwnerUserID: uuid.NewV7()})
	problemtest.WantErr(t, err, kvkk.ErrUnknownUser)
	_, err = e.svc.CreateBreach(ctx, actor(t, "moderator"), kvkk.BreachInput{})
	problemtest.WantErr(t, err, authz.ErrForbidden)
}

// TestBreach_UpdateRules: If-Match; detected_at only earlier (the deadline
// follows); notification times written once; status forward only;
// notified needs the Board time; closed is final.
func TestBreach_UpdateRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, pad := t.Context(), actor(t, "platform_admin")
	b := e.newBreach(t, e.clk.Now().Add(-2*time.Hour))
	etag := func(x kvkk.Breach) string { return kvkk.ETagOf(x.UpdatedAt) }
	contained := kvkk.BreachContained
	_, err := e.svc.UpdateBreach(ctx, pad, b.ID, "", kvkk.BreachPatch{Status: &contained})
	problemtest.WantErr(t, err, problem.ErrPreconditionRequired)
	later := b.DetectedAt.Add(time.Hour)
	_, err = e.svc.UpdateBreach(ctx, pad, b.ID, etag(b), kvkk.BreachPatch{DetectedAt: &later})
	problemtest.WantErr(t, err, kvkk.ErrInvalidRequest)
	earlier := b.DetectedAt.Add(-3 * time.Hour)
	ctrl := e.clk.Now().Add(-time.Hour)
	b2, err := e.svc.UpdateBreach(ctx, pad, b.ID, etag(b), kvkk.BreachPatch{DetectedAt: &earlier, Status: &contained,
		ControllerNotifiedAt: &ctrl})
	if err != nil || !b2.AuthorityDeadlineAt.Equal(earlier.Add(72*time.Hour)) || b2.Status != kvkk.BreachContained {
		t.Fatalf("update = %+v, %v", b2, err)
	}
	_, err = e.svc.UpdateBreach(ctx, pad, b.ID, etag(b), kvkk.BreachPatch{Status: &contained})
	problemtest.WantErr(t, err, problem.ErrPreconditionFailed)
	other := ctrl.Add(-time.Minute)
	_, err = e.svc.UpdateBreach(ctx, pad, b.ID, etag(b2), kvkk.BreachPatch{ControllerNotifiedAt: &other})
	problemtest.WantErr(t, err, kvkk.ErrState) // written once
	open := kvkk.BreachOpen
	_, err = e.svc.UpdateBreach(ctx, pad, b.ID, etag(b2), kvkk.BreachPatch{Status: &open})
	problemtest.WantErr(t, err, kvkk.ErrState) // forward only
	notified := kvkk.BreachNotified
	_, err = e.svc.UpdateBreach(ctx, pad, b.ID, etag(b2), kvkk.BreachPatch{Status: &notified})
	problemtest.WantErr(t, err, kvkk.ErrInvalidRequest) // needs the Board time
	auth := e.clk.Now()
	b3, err := e.svc.UpdateBreach(ctx, pad, b.ID, etag(b2), kvkk.BreachPatch{Status: &notified, AuthorityNotifiedAt: &auth})
	if err != nil || b3.Status != kvkk.BreachNotified {
		t.Fatalf("notified = %+v, %v", b3, err)
	}
	closed := kvkk.BreachClosed
	b4, err := e.svc.UpdateBreach(ctx, pad, b.ID, etag(b3), kvkk.BreachPatch{Status: &closed})
	if err != nil || b4.ClosedAt == nil {
		t.Fatalf("closed = %+v, %v", b4, err)
	}
	title := "Yeni başlık"
	_, err = e.svc.UpdateBreach(ctx, pad, b.ID, etag(b4), kvkk.BreachPatch{Title: &title})
	problemtest.WantErr(t, err, kvkk.ErrState)
	_, err = e.svc.UpdateBreach(ctx, pad, uuid.NewV7(), etag(b4), kvkk.BreachPatch{Title: &title})
	problemtest.WantErr(t, err, kvkk.ErrNotFound)
}

// TestBreachTimer_WarnsAt48_24_6 (01 §7 kvkk.breach_timer): hourly, the
// Board deadline is warned once at 48, 24 and 6 hours left and once when
// overdue; the controller notice at 12 and 6 hours left and when overdue;
// recording the notification stops its warnings; a closed breach is quiet.
func TestBreachTimer_WarnsAt48_24_6(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	job := e.job(t, kvkk.BreachTimerJobName)
	if job.Cron != "0 * * * *" || job.Queue != "critical" {
		t.Fatalf("breach timer = %s %s", job.Cron, job.Queue)
	}
	start := e.clk.Now()
	b := e.newBreach(t, start)
	// marks returns the "<step>:<hours>" warnings mailed to the DPO so far.
	marks := func() []string {
		var out []string
		for _, m := range e.mail.of("kvkk.breach_deadline") {
			if m.Channel == notify.ChannelEmail {
				out = append(out, m.Data["step"].(string)+":"+strconv.Itoa(m.Data["hours_left"].(int)))
			}
		}
		return out
	}
	runAt := func(h int) {
		t.Helper()
		e.clk.Set(start.Add(time.Duration(h) * time.Hour))
		if err := job.Handle(workerCtx(t)); err != nil {
			t.Fatal(err)
		}
	}
	for h := range 12 {
		runAt(h)
	}
	if got := marks(); len(got) != 0 {
		t.Fatalf("warnings before any mark = %v", got)
	}
	for h := 12; h <= 25; h++ { // 24h controller deadline: 12 left at h12, 6 at h18, overdue at h24
		runAt(h)
	}
	if got := marks(); !slices.Equal(got, []string{"controller:12", "controller:6", "controller:0", "authority:48"}) {
		t.Fatalf("after 25 hours: %v", got)
	}
	// The controller was told: its warnings stop.
	at := start.Add(25 * time.Hour)
	cur, _ := e.records.Breach(t.Context(), b.ID)
	if _, err := e.svc.UpdateBreach(t.Context(), actor(t, "dpo"), b.ID, kvkk.ETagOf(cur.UpdatedAt),
		kvkk.BreachPatch{ControllerNotifiedAt: &at}); err != nil {
		t.Fatal(err)
	}
	for h := 26; h <= 80; h++ {
		runAt(h)
	}
	want := []string{"controller:12", "controller:6", "controller:0", "authority:48", "authority:24", "authority:6", "authority:0"}
	if got := marks(); !slices.Equal(got, want) {
		t.Fatalf("warnings = %v, want %v (each once)", got, want)
	}
	// In-app copies go to the platform admins and the owner, with the ID only.
	for _, m := range e.mail.of("kvkk.breach_deadline") {
		if m.Channel == notify.ChannelInApp && (len(m.Data) != 1 || m.LinkPath.String() != "/admin/breaches") {
			t.Fatalf("in-app warning = %+v", m)
		}
	}
	// A closed breach is quiet; a late-recorded one (5 h left) gets only the tightest mark.
	late := e.newBreach(t, e.clk.Now().Add(-67*time.Hour))
	n := len(e.mail.of("kvkk.breach_deadline"))
	runAt(81)
	got := e.mail.of("kvkk.breach_deadline")[n:]
	var lateMarks []string
	for _, m := range got {
		if m.Channel == notify.ChannelEmail && m.Data["breach_id"] == late.ID.String() {
			lateMarks = append(lateMarks, m.Data["step"].(string)+":"+strconv.Itoa(m.Data["hours_left"].(int)))
		}
	}
	if !slices.Equal(lateMarks, []string{"controller:0", "authority:6"}) {
		t.Fatalf("late breach warnings = %v", lateMarks)
	}
}

// TestSubprocessor_Rules (02 §16.5): validation, unique name, domestic only
// in Turkey, If-Match, retire by active = false; dpo only manages.
func TestSubprocessor_Rules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, dpo := t.Context(), actor(t, "dpo")
	signed := civil.Date{Year: 2026, Month: time.September, Day: 1}
	in := kvkk.SubprocessorInput{Name: "Bulut A.Ş.", Service: "Nesne depolama", DataCategories: []string{"files"},
		Region: "eu-west-1", CountryCode: "ie", TransferMechanism: "standard_contract", ContractSignedAt: &signed}
	x, err := e.svc.CreateSubprocessor(ctx, dpo, in)
	if err != nil || x.CountryCode != "IE" || !x.Active {
		t.Fatalf("create = %+v, %v", x, err)
	}
	_, err = e.svc.CreateSubprocessor(ctx, dpo, in)
	problemtest.WantErr(t, err, kvkk.ErrDuplicateSubprocessor)
	abroad := in
	abroad.Name, abroad.TransferMechanism = "Yurt dışı", "domestic"
	_, err = e.svc.CreateSubprocessor(ctx, dpo, abroad)
	problemtest.WantErr(t, err, kvkk.ErrInvalidRequest)
	future := civil.Date{Year: 2030, Month: time.January, Day: 1}
	bad := in
	bad.Name, bad.ContractSignedAt = "Gelecek", &future
	_, err = e.svc.CreateSubprocessor(ctx, dpo, bad)
	problemtest.WantErr(t, err, kvkk.ErrInvalidRequest)
	_, err = e.svc.CreateSubprocessor(ctx, actor(t, "platform_admin"), in)
	problemtest.WantErr(t, err, authz.ErrForbidden)

	no := false
	_, err = e.svc.UpdateSubprocessor(ctx, dpo, x.ID, "", kvkk.SubprocessorPatch{Active: &no})
	problemtest.WantErr(t, err, problem.ErrPreconditionRequired)
	y, err := e.svc.UpdateSubprocessor(ctx, dpo, x.ID, kvkk.ETagOf(x.UpdatedAt), kvkk.SubprocessorPatch{Active: &no})
	if err != nil || y.Active {
		t.Fatalf("retire = %+v, %v", y, err)
	}
	_, err = e.svc.UpdateSubprocessor(ctx, dpo, x.ID, kvkk.ETagOf(x.UpdatedAt), kvkk.SubprocessorPatch{Active: &no})
	problemtest.WantErr(t, err, problem.ErrPreconditionFailed)
	yes := true
	active, err := e.svc.ListSubprocessors(ctx, actor(t, "platform_admin"), &yes)
	if err != nil || len(active) != 0 {
		t.Fatalf("active list = %v, %v", active, err)
	}
	if all, _ := e.svc.ListSubprocessors(ctx, dpo, nil); len(all) != 1 {
		t.Fatalf("full list = %v", all)
	}
}

// TestErasure_SessionsRevokedAtApproval_DestructionLogged (06 H-49, 02
// §16.3): approving an erasure signs the subject out after the commit, the
// run revokes again before it starts, and the run writes one dsr_erasure
// destruction row per module, keyed by the request.
func TestErasure_SessionsRevokedAtApproval_DestructionLogged(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	student := actor(t, "student")
	d, err := e.svc.CreateMine(t.Context(), student, kvkk.MyRequest{Type: kvkk.TypeErasure, Proof: proof})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.revoked.all(); len(got) != 0 {
		t.Fatalf("revoked at the request already: %v", got)
	}
	e.completeAs(t, d.ID)
	if got := e.revoked.all(); !slices.Equal(got, []string{"revoke:" + student.UserID.String()}) {
		t.Fatalf("revocations after the approval = %v", got)
	}
	if err := e.deliverTopic(t, domainevent.TopicDSRErasureApproved); err != nil {
		t.Fatal(err)
	}
	if got := e.revoked.all(); len(got) != 2 {
		t.Fatalf("revocations after the run = %v, want 2 (approval + run start)", got)
	}
	logs, err := e.records.DestructionLogsOfRun(t.Context(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	var modules []string
	for _, l := range logs {
		if l.PolicyCode != kvkk.ErasurePolicyCode {
			t.Fatalf("erasure row policy = %s", l.PolicyCode)
		}
		modules = append(modules, l.Module)
	}
	slices.Sort(modules)
	if !slices.Equal(modules, []string{"consent", "events", "identity", "kvkk"}) {
		t.Fatalf("erasure rows = %v, want one per provider", modules)
	}
	// A redelivered event changes nothing.
	if err := e.deliverTopic(t, domainevent.TopicDSRErasureApproved); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.records.DestructionLogsOfRun(t.Context(), d.ID); len(again) != len(logs) {
		t.Fatalf("redelivery wrote %d more rows", len(again)-len(logs))
	}
}

// TestHTTP_ListBreaches_PagesAndHoursLeft (BE-129): adminListBreaches pages
// the register (the most recently detected first; limit, cursor) and
// shows the whole hours left to each notification not made yet: the Board's
// (72 h) and the data controller's (24 h), negative once overdue, absent
// once made or closed.
func TestHTTP_ListBreaches_PagesAndHoursLeft(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	now := e.clk.Now()
	old := e.newBreach(t, now.Add(-30*time.Hour))    // controller notice overdue by 6 h
	fresh := e.newBreach(t, now.Add(-5*time.Hour))   // 19 h and 67 h left
	middle := e.newBreach(t, now.Add(-10*time.Hour)) // the controller was told already
	at := now.Add(-time.Hour)
	if _, err := e.svc.UpdateBreach(t.Context(), actor(t, "dpo"), middle.ID, kvkk.ETagOf(middle.UpdatedAt),
		kvkk.BreachPatch{ControllerNotifiedAt: &at}); err != nil {
		t.Fatal(err)
	}
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(authztest.Personas()...)})
	if err != nil {
		t.Fatal(err)
	}
	kvkk.Register(app.V1, e.svc, e.clk)
	list := func(persona, query string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/admin/breaches"+query, nil)
		pp, _ := authztest.Get(persona)
		req.Header.Set("Authorization", "Bearer "+pp.Token)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	var got []map[string]any
	query := "?limit=2"
	for range 3 {
		code, out := list("dpo", query)
		if code != http.StatusOK {
			t.Fatalf("list %s = %d %v", query, code, out)
		}
		for _, it := range out["items"].([]any) {
			got = append(got, it.(map[string]any))
		}
		next, _ := out["next_cursor"].(string)
		if next == "" {
			break
		}
		query = "?limit=2&cursor=" + url.QueryEscape(next)
	}
	if len(got) != 3 || got[0]["breach_id"] != fresh.ID.String() || got[1]["breach_id"] != middle.ID.String() ||
		got[2]["breach_id"] != old.ID.String() {
		t.Fatalf("pages = %v", got)
	}
	hours := func(it map[string]any, key string) any { return it[key] }
	if hours(got[0], "controller_hours_left") != float64(19) || hours(got[0], "authority_hours_left") != float64(67) ||
		hours(got[1], "controller_hours_left") != nil || hours(got[1], "authority_hours_left") != float64(62) ||
		hours(got[2], "controller_hours_left") != float64(-6) || hours(got[2], "authority_hours_left") != float64(42) {
		t.Fatalf("hours left = %v", got)
	}
	if code, _ := list("moderator", ""); code != http.StatusForbidden {
		t.Fatalf("moderator = %d", code)
	}
}
