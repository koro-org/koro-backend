package events_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/xuri/excelize/v2"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/httpx"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
)

// exportWhy is a valid export reason.
const exportWhy = "Kapıda yoklama ve sertifika listesi için gerekli."

// allergy is a special-category answer (warn_special): it lives encrypted
// and never reaches an export.
const allergy = "Fıstık alerjim var"

// good is the right re-authentication proof of every test user.
var good = identity.ReauthProof{Password: goodPassword}

// exportEvent publishes an event of club_a with a form of a long text, a
// select, a warn_special field and a consent field, and registers
// personas (each answering the special-category field too).
func (e *env) exportEvent(t *testing.T, regs ...string) events.EventView {
	t.Helper()
	ev := e.regEvent(t, nil, motivationField, tshirtField, allergyField)
	for _, name := range regs {
		e.mustRegister(t, persona(t, name), ev.Event.ID, map[string]any{
			"motivation": "=HYPERLINK(\"http://kotu.example.test\")", "tshirt": "m", "allergy": allergy,
		})
	}
	return ev
}

// export asks for an export of eventID as club_admin with the right proof.
func (e *env) export(t *testing.T, eventID uuid.UUID, in events.ExportInput) events.Export {
	t.Helper()
	x, err := e.svc.CreateExport(t.Context(), persona(t, "club_admin"), eventID, in, good)
	if err != nil {
		t.Fatalf("CreateExport: %v %v", err, fieldsOf(err))
	}
	return x
}

// build runs the queued build tasks (the worker's part).
func (e *env) build(t *testing.T) {
	t.Helper()
	var handle func(context.Context, []byte) error
	for _, tk := range e.mod.Tasks() {
		if tk.Name == events.BuildExportTaskName {
			handle = tk.Handle
		}
	}
	for _, q := range e.queue.Tasks() {
		if q.Type != events.BuildExportTaskName {
			continue
		}
		if err := handle(t.Context(), q.Payload); err != nil {
			t.Fatalf("%s: %v", events.BuildExportTaskName, err)
		}
	}
	e.queue.Reset()
}

// stored reads an export through the store.
func (e *env) stored(t *testing.T, x events.Export) events.Export {
	t.Helper()
	got, err := e.mem.GetExport(t.Context(), x.OrgID, x.EventID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// content is the file of a ready export.
func (e *env) content(t *testing.T, x events.Export) []byte {
	t.Helper()
	x = e.stored(t, x)
	if x.Status != events.ExportReady || x.FileID == nil {
		t.Fatalf("export %s is %s", x.ID, x.Status)
	}
	b, meta, err := e.files.Read(t.Context(), *x.FileID, files.PurposeEventExport.MaxBytes())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Purpose != files.PurposeEventExport {
		t.Fatalf("file purpose = %s", meta.Purpose)
	}
	return b
}

// csvRows parses a CSV export (the byte order mark first).
func csvRows(t *testing.T, b []byte) [][]string {
	t.Helper()
	body, ok := bytes.CutPrefix(b, []byte("\xEF\xBB\xBF"))
	if !ok {
		t.Fatal("the CSV export has no UTF-8 byte order mark")
	}
	r := csv.NewReader(bytes.NewReader(body))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// xlsxRows reads the worksheet of an XLSX export.
func xlsxRows(t *testing.T, b []byte) [][]string {
	t.Helper()
	f, err := excelize.OpenReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sheets := f.GetSheetList()
	if len(sheets) != 1 {
		t.Fatalf("sheets = %v", sheets)
	}
	rows, err := f.GetRows(sheets[0])
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// rowsOf returns the recorded events of act in audittest's terms.
func (l *auditLog) rowsOf(act authz.Action) []audittest.Row {
	var out []audittest.Row
	for _, e := range l.of(act) {
		out = append(out, audittest.Row{Action: e.Action, Outcome: string(e.Outcome), Reason: e.Reason,
			ActorUserID: e.ActorUserID, ActorOrgID: e.ActorOrgID, SystemJob: e.SystemJob, OrgID: e.OrgID,
			ResourceType: e.ResourceType, ResourceID: e.ResourceID, Metadata: e.Metadata})
	}
	return out
}

// TestCreateExport_IncludeSensitive_422 (T-37): special-category answers
// never leave Koro. include_sensitive is refused with 422 before the proof
// is checked and nothing is written; a special-category or consent field
// is not on the allow-list either. Without it the same request is queued.
func TestCreateExport_IncludeSensitive_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student")
	a := persona(t, "club_admin")
	in := events.ExportInput{Reason: exportWhy, Fields: []string{"first_name", "answers.motivation"}, IncludeSensitive: true}
	_, err := e.svc.CreateExport(t.Context(), a, ev.Event.ID, in, identity.ReauthProof{})
	wantField(t, err, 422, "validation_failed", "body.include_sensitive")
	for _, f := range []string{"answers.allergy", "answers.share_sponsor"} {
		_, err := e.svc.CreateExport(t.Context(), a, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{f}}, good)
		wantField(t, err, 422, "validation_failed", "body.fields[0]")
	}
	if n := len(e.audit.of(authz.EventsExportCreate)); n != 0 {
		t.Fatalf("%d audit records for refused bodies (the proof was never checked)", n)
	}
	if p, _ := e.svc.ListExports(t.Context(), a, ev.Event.ID, 10, nil); len(p.Items) != 0 {
		t.Fatalf("refused requests stored %d exports", len(p.Items))
	}
	in.IncludeSensitive = false
	x := e.export(t, ev.Event.ID, in)
	if x.Status != events.ExportQueued || x.IncludeSensitive || x.Format != events.FormatXLSX {
		t.Fatalf("export = %+v", x)
	}
	e.build(t)
	if b := e.content(t, x); bytes.Contains(b, []byte("Fıstık")) || bytes.Contains(b, []byte("allergy")) {
		t.Fatal("the special-category answer reached the file")
	}
}

// TestCreateExport_RequiresReauthAndReason (03 §6.2, 04 §2.6): a reason of
// 10–500 characters and at least one field are required (422); the proof
// is required (403 identity.reauth_required) and must be right (403
// identity.reauth_failed), each refusal one denied record of the export to
// be (org from the event row, no business data); the right proof queues
// the export with its audit record (the reason, the fields, the proof's
// kind), the requester's IP and the build task after the commit. Without
// identity wired the operation answers 503.
func TestCreateExport_RequiresReauthAndReason(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student")
	a := persona(t, "club_admin")
	ctx := httpx.WithClientIP(t.Context(), "::ffff:198.51.100.23")
	for _, tc := range []struct {
		in  events.ExportInput
		loc string
	}{
		{events.ExportInput{Reason: "kısa", Fields: []string{"first_name"}}, "body.reason"},
		{events.ExportInput{Reason: strings.Repeat("a", 501), Fields: []string{"first_name"}}, "body.reason"},
		{events.ExportInput{Reason: exportWhy}, "body.fields"},
		{events.ExportInput{Reason: exportWhy, Fields: []string{"status", "status"}}, "body.fields[1]"},
		{events.ExportInput{Reason: exportWhy, Fields: []string{"status"}, Format: "pdf"}, "body.format"},
	} {
		_, err := e.svc.CreateExport(ctx, a, ev.Event.ID, tc.in, good)
		wantField(t, err, 422, "validation_failed", tc.loc)
	}
	in := events.ExportInput{Reason: exportWhy, Fields: []string{"first_name", "last_name", "status"}, Format: events.FormatCSV}
	org := ev.Event.OrgID
	denied := audittest.Row{Action: string(authz.EventsExportCreate), ActorUserID: &a.UserID, ResourceType: authz.TypeEventExport, OrgID: &org}
	for _, tc := range []struct {
		proof identity.ReauthProof
		want  error
	}{{identity.ReauthProof{}, identity.ErrReauthRequired}, {identity.ReauthProof{Password: "yanlis-parola-1"}, identity.ErrReauthFailed}} {
		n := len(e.audit.rowsOf(authz.EventsExportCreate))
		if _, err := e.svc.CreateExport(ctx, a, ev.Event.ID, in, tc.proof); !errors.Is(err, tc.want) {
			t.Fatalf("proof %v: %v, want %v", tc.proof, err, tc.want)
		}
		audittest.WantDenied(t, e.audit.rowsOf(authz.EventsExportCreate)[n:], denied)
	}
	if len(e.queue.Tasks()) != 0 {
		t.Fatal("a refused request queued a build")
	}
	x, err := e.svc.CreateExport(ctx, a, ev.Event.ID, in, good)
	if err != nil {
		t.Fatal(err)
	}
	if x.Status != events.ExportQueued || x.RequestedBy != a.UserID || x.Reason != exportWhy || x.IP == nil ||
		*x.IP != "198.51.100.23" || !x.ExpiresAt.Equal(t0.Add(events.ExportLifetime)) || x.Format != events.FormatCSV {
		t.Fatalf("export = %+v", x)
	}
	recs := e.audit.of(authz.EventsExportCreate)
	last := recs[len(recs)-1]
	if last.Outcome != audit.Success || last.Reason != exportWhy || last.Metadata[audit.MetaReauth] != "password" ||
		*last.ResourceID != x.ID || *last.OrgID != org || !slices.Equal(last.Metadata["fields"].([]string), in.Fields) {
		t.Fatalf("audit record = %+v", last)
	}
	tasks := e.queue.Tasks()
	if len(tasks) != 1 || tasks[0].Type != events.BuildExportTaskName || tasks[0].ID != events.BuildExportTaskName+":"+x.ID.String() ||
		bytes.Contains(tasks[0].Payload, []byte(exportWhy)) {
		t.Fatalf("tasks = %+v (one build task, IDs only)", tasks)
	}
	// Who may: the managers of the organiser; a visible event without the
	// role is 403, an invisible one 404.
	for _, who := range []string{"club_member", "event_staff", "support", "dpo"} {
		_, err := e.svc.CreateExport(ctx, persona(t, who), ev.Event.ID, in, good)
		wantForbidden(t, err)
	}
	_, err = e.svc.CreateExport(ctx, a, uuid.NewV7(), in, good)
	wantNotFound(t, err)
	// A suspended organiser exports nothing (03 §2.3).
	e.orgs.setStatus(orgID("club_a"), "suspended")
	_, err = e.svc.CreateExport(ctx, a, ev.Event.ID, in, good)
	wantCode(t, err, 403, "orgs.org_suspended")
	e.orgs.setStatus(orgID("club_a"), "active")
	// Without identity (or files, or the queue) nothing can be exported.
	noReauth := newEnv(t, func(d *events.Deps) { d.Reauth = nil })
	ev2 := noReauth.exportEvent(t)
	_, err = noReauth.svc.CreateExport(ctx, a, ev2.Event.ID, in, good)
	wantCode(t, err, 503, "unavailable")
}

// TestReauthDenied_EveryROperation (03 §6.2, BE-074): events' R operation
// (events.export.create) audits a refused proof — wrong or missing — as
// one denied reauth_failed record of the export to be with the event's org
// (read before the proof, 06 H-38) and no business data; an overloaded
// server writes nothing; the right proof afterwards goes through.
func TestReauthDenied_EveryROperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student")
	a := persona(t, "club_admin")
	org := ev.Event.OrgID
	in := events.ExportInput{Reason: exportWhy, Fields: []string{"registration_id"}}
	audittest.ReauthCase[identity.ReauthProof]{
		Call: func(ctx context.Context, p identity.ReauthProof) error {
			_, err := e.svc.CreateExport(authz.WithActor(ctx, a), a, ev.Event.ID, in, p)
			return err
		},
		Rows: func() []audittest.Row { return e.audit.rowsOf(authz.EventsExportCreate) },
		Denied: audittest.Row{Action: string(authz.EventsExportCreate), ActorUserID: &a.UserID,
			ResourceType: authz.TypeEventExport, OrgID: &org},
		Late:  e.reauth,
		Wrong: identity.ReauthProof{Password: "yanlis-parola-1"}, WrongErr: identity.ErrReauthFailed,
		Missing: identity.ReauthProof{}, MissingErr: identity.ErrReauthRequired,
		Good: good,
	}.Run(t)
}

// TestExport_NoEmailColumn (T-69): the organiser never gets the
// registrants' e-mail: no field of the allow-list is one (an e-mail field
// is 422), and a file with every allowed column holds no address, no
// special-category answer, and neutralised formulas (05 §14.6). Both
// formats carry the personal-data warning, then the header.
func TestExport_NoEmailColumn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student", "club_member")
	for _, f := range []string{"email", "user.email", "answers.email"} {
		_, err := e.svc.CreateExport(t.Context(), persona(t, "club_admin"), ev.Event.ID,
			events.ExportInput{Reason: exportWhy, Fields: []string{"first_name", f}}, good)
		wantField(t, err, 422, "validation_failed", "body.fields[1]")
	}
	fields := append(events.ExportBaseFields(), "answers.motivation", "answers.tshirt")
	csvX := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: fields, Format: events.FormatCSV})
	xlsxX := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: fields, Format: events.FormatXLSX})
	e.build(t)
	for name, rows := range map[string][][]string{"csv": csvRows(t, e.content(t, csvX)), "xlsx": xlsxRows(t, e.content(t, xlsxX))} {
		if len(rows) != 4 {
			t.Fatalf("%s: %d rows, want warning + header + 2 registrations", name, len(rows))
		}
		if !strings.HasPrefix(rows[0][0], "KİŞİSEL VERİ") {
			t.Fatalf("%s: first row %q is not the personal-data warning", name, rows[0])
		}
		if !slices.Equal(rows[1], fields) {
			t.Fatalf("%s: header %q, want %q", name, rows[1], fields)
		}
		for _, h := range rows[1] {
			if strings.Contains(strings.ToLower(h), "mail") {
				t.Fatalf("%s: e-mail column %q", name, h)
			}
		}
		var all strings.Builder
		for _, r := range rows[2:] {
			all.WriteString(strings.Join(r, "|") + "\n")
			if r[slices.Index(fields, "first_name")] != "Ayla" || r[slices.Index(fields, "last_name")] != "Yılmaz" {
				t.Fatalf("%s: row %q lacks the registrant's name", name, r)
			}
			if got := r[slices.Index(fields, "answers.motivation")]; !strings.HasPrefix(got, "'=") {
				t.Fatalf("%s: formula cell %q not neutralised", name, got)
			}
			if r[slices.Index(fields, "answers.tshirt")] != "m" || r[slices.Index(fields, "status")] != "confirmed" {
				t.Fatalf("%s: row %q", name, r)
			}
		}
		if strings.Contains(all.String(), "@") || strings.Contains(all.String(), "Fıstık") {
			t.Fatalf("%s: the file holds an e-mail address or a special-category answer:\n%s", name, all.String())
		}
	}
	if x := e.stored(t, csvX); x.RowCount == nil || *x.RowCount != 2 || len(x.FileSHA256) != 32 {
		t.Fatalf("ready export = %+v", x)
	}
}

// TestDownloadExport_NotReady_409 (04 §2.6): only a ready export is
// downloaded: queued, failed or purged (and a ready one past expires_at
// before the hourly purge) is 409 events.export_not_ready, without a
// counted download or an audit record.
func TestDownloadExport_NotReady_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student")
	a := persona(t, "club_admin")
	x := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"first_name"}})
	_, err := e.svc.DownloadExport(t.Context(), a, ev.Event.ID, x.ID)
	wantCode(t, err, 409, "events.export_not_ready")
	e.build(t)
	if _, err := e.svc.DownloadExport(t.Context(), a, ev.Event.ID, x.ID); err != nil {
		t.Fatalf("ready export: %v", err)
	}
	e.clk.Advance(events.ExportLifetime)
	_, err = e.svc.DownloadExport(t.Context(), a, ev.Event.ID, x.ID)
	wantCode(t, err, 409, "events.export_not_ready")
	e.job(t, events.PurgeExportsJobName)
	_, err = e.svc.DownloadExport(t.Context(), a, ev.Event.ID, x.ID)
	wantCode(t, err, 409, "events.export_not_ready")
	// A build that never ran is failed after a day: 409 too.
	stale := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"status"}})
	e.queue.Reset()
	e.clk.Advance(events.ExportGiveUpAfter + time.Minute)
	e.job(t, events.PurgeExportsJobName)
	if got := e.stored(t, stale); got.Status != events.ExportFailed {
		t.Fatalf("stale export = %s, want failed", got.Status)
	}
	_, err = e.svc.DownloadExport(t.Context(), a, ev.Event.ID, stale.ID)
	wantCode(t, err, 409, "events.export_not_ready")
	if n := len(e.audit.of(authz.EventsExportDownload)); n != 1 {
		t.Fatalf("%d download records, want only the ready one's", n)
	}
	if got := e.stored(t, x); got.DownloadCount != 1 {
		t.Fatalf("download_count = %d, want 1", got.DownloadCount)
	}
}

// TestDownloadExport_IncrementsCountAndAudits (03 §5.6, §6.5): every
// download of the requester counts (download_count, first_downloaded_at
// once) and is audited before the URL (≤ 5 minutes) is returned; another
// manager of the organiser (not the requester) gets 403, a non-manager
// 403, another event's export ID 404 (06 H-38).
func TestDownloadExport_IncrementsCountAndAudits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student")
	a := persona(t, "club_admin")
	x := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"first_name"}})
	e.build(t)
	first := t0
	dl, err := e.svc.DownloadExport(t.Context(), a, ev.Event.ID, x.ID)
	if err != nil || dl.URL == "" || dl.ExpiresAt.After(first.Add(events.ExportDownloadTTL)) {
		t.Fatalf("download = %+v, %v", dl, err)
	}
	e.clk.Advance(time.Hour)
	if _, err := e.svc.DownloadExport(t.Context(), a, ev.Event.ID, x.ID); err != nil {
		t.Fatal(err)
	}
	got := e.stored(t, x)
	if got.DownloadCount != 2 || got.FirstDownloadedAt == nil || !got.FirstDownloadedAt.Equal(first) {
		t.Fatalf("export after two downloads = %+v", got)
	}
	recs := e.audit.of(authz.EventsExportDownload)
	if len(recs) != 2 {
		t.Fatalf("%d download records, want 2", len(recs))
	}
	for i, r := range recs {
		if r.Outcome != audit.Success || *r.ResourceID != x.ID || *r.ActorUserID != a.UserID || *r.OrgID != ev.Event.OrgID ||
			r.Metadata["download_count"] != i+1 || r.Metadata["file_id"] != got.FileID.String() {
			t.Fatalf("download record %d = %+v", i, r)
		}
	}
	// Another club_admin of club_a (not the requester): 403 (S·O).
	other := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), SessionID: uuid.NewV7(),
		Memberships: []authz.Membership{{OrgID: orgID("club_a"), OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}}
	_, err = e.svc.DownloadExport(t.Context(), other, ev.Event.ID, x.ID)
	wantForbidden(t, err)
	for _, who := range []string{"student", "club_member", "dpo", "platform_admin"} {
		_, err := e.svc.DownloadExport(t.Context(), persona(t, who), ev.Event.ID, x.ID)
		wantForbidden(t, err)
	}
	// The export is read through its own event only.
	ev2 := e.exportEvent(t)
	_, err = e.svc.DownloadExport(t.Context(), a, ev2.Event.ID, x.ID)
	wantNotFound(t, err)
	if n := len(e.audit.of(authz.EventsExportDownload)); n != 2 {
		t.Fatalf("refused downloads wrote records: %d", n)
	}
	// The list shows the counts; the DPO oversees it, the student does not.
	p, err := e.svc.ListExports(t.Context(), persona(t, "dpo"), ev.Event.ID, 10, nil)
	if err != nil || len(p.Items) != 1 || p.Items[0].DownloadCount != 2 || p.Items[0].RowCount == nil {
		t.Fatalf("dpo list = %+v, %v", p, err)
	}
	_, err = e.svc.ListExports(t.Context(), persona(t, "student"), ev.Event.ID, 10, nil)
	wantForbidden(t, err)
}

// TestPurgeExports_After7Days (01 §7 events.purge_exports, 02 §24.2
// event_export_file): the file of a ready export lives until expires_at
// (7 days); the hourly job then deletes it (row purged, file_id cleared,
// files row and objects gone) with an events.export.purge record, never
// earlier and only once. A queued export whose build task was lost is
// queued again; the row itself stays (the 2-year log).
func TestPurgeExports_After7Days(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student")
	x := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"first_name"}, Format: events.FormatCSV})
	e.build(t)
	ready := e.stored(t, x)
	fileID := *ready.FileID
	objects := e.objects.Len()
	e.clk.Advance(events.ExportLifetime - time.Minute)
	e.job(t, events.PurgeExportsJobName)
	if got := e.stored(t, x); got.Status != events.ExportReady || got.FileID == nil {
		t.Fatalf("export purged before 7 days: %+v", got)
	}
	e.clk.Advance(time.Minute)
	e.job(t, events.PurgeExportsJobName)
	got := e.stored(t, x)
	if got.Status != events.ExportPurged || got.FileID != nil || got.PurgedAt == nil || !got.PurgedAt.Equal(e.clk.Now()) {
		t.Fatalf("export after 7 days = %+v", got)
	}
	if _, err := e.files.Get(t.Context(), fileID); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("the export file still exists: %v", err)
	}
	if n := e.objects.Len(); n != objects-1 {
		t.Fatalf("objects = %d, want %d (the file's object deleted)", n, objects-1)
	}
	recs := e.audit.of(authz.EventsExportPurge)
	if len(recs) != 1 || recs[0].SystemJob != events.PurgeExportsJobName || *recs[0].ResourceID != x.ID ||
		recs[0].Metadata["policy_code"] != events.PolicyExportFile || recs[0].Metadata["file_id"] != fileID.String() {
		t.Fatalf("purge records = %+v", recs)
	}
	e.job(t, events.PurgeExportsJobName)
	if n := len(e.audit.of(authz.EventsExportPurge)); n != 1 {
		t.Fatalf("a second run purged again: %d records", n)
	}
	// A lost build task (Redis down at the commit): queued again after 15
	// minutes, then built.
	e.queue.Fail(errors.New("redis down"))
	lost := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"status"}})
	e.queue.Fail(nil)
	e.job(t, events.PurgeExportsJobName)
	if len(e.queue.Tasks()) != 0 {
		t.Fatal("a fresh queued export was queued again")
	}
	e.clk.Advance(events.ExportRequeueAfter + time.Second)
	e.job(t, events.PurgeExportsJobName)
	e.build(t)
	if got := e.stored(t, lost); got.Status != events.ExportReady {
		t.Fatalf("lost build = %s, want ready after the repair", got.Status)
	}
	// The organiser's list still shows the purged export (the log).
	p, err := e.svc.ListExports(t.Context(), persona(t, "club_admin"), ev.Event.ID, 10, nil)
	if err != nil || len(p.Items) != 2 {
		t.Fatalf("list = %+v, %v", p, err)
	}
}

// TestExport_PrivacyAndRetention (01 §3.3, 02 §24.2): the requester's KVKK
// export lists their export requests (never the file); the erasure keeps
// the rows (the organisation's accountability record) without the IP; the
// events sweeper claims event_export_log.
func TestExport_PrivacyAndRetention(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student")
	ctx := httpx.WithClientIP(t.Context(), "203.0.113.9")
	a := persona(t, "club_admin")
	x, err := e.svc.CreateExport(ctx, a, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"first_name"}}, good)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := e.mod.Privacy.Export(t.Context(), a.UserID)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(sec.Items, func(m map[string]any) bool { return m["type"] == "event_data_export" })
	if i < 0 || sec.Items[i]["export_id"] != x.ID.String() || sec.Items[i]["reason"] != exportWhy || sec.Items[i]["ip"] != "203.0.113.9" {
		t.Fatalf("KVKK export = %+v", sec.Items)
	}
	res, err := e.mod.Privacy.Erase(t.Context(), a.UserID)
	if err != nil || res.Anonymized != 1 {
		t.Fatalf("Erase = %+v, %v", res, err)
	}
	if got := e.stored(t, x); got.IP != nil || got.Reason != exportWhy {
		t.Fatalf("erased requester's export = %+v (row kept, IP gone)", got)
	}
	if !slices.Contains(e.mod.Sweeper.PolicyCodes(), events.PolicyExportLog) {
		t.Fatalf("sweeper codes = %v", e.mod.Sweeper.PolicyCodes())
	}
}

// TestSweepExportLog_After2Years (02 §24.2 event_export_log, BE-057): the
// export rows go 2 years after the request through kvkk.retention_sweep
// (a dry run while the policy is not legally approved); a row still
// holding its file waits for the file purge.
func TestSweepExportLog_After2Years(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student")
	x := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"first_name"}})
	e.build(t)
	day := 24 * time.Hour
	p := privacy.RetentionPolicy{Code: events.PolicyExportLog, Retention: 730 * day}
	at := t0.Add(731 * day)
	if res, err := e.mod.Sweeper.Sweep(t.Context(), p, at); err != nil || res.Count != 0 || res.Table != "event_data_exports" ||
		res.Method != privacy.MethodHardDelete {
		t.Fatalf("sweep of a ready export = %+v, %v (its file is not purged yet)", res, err)
	}
	e.clk.Advance(events.ExportLifetime)
	e.job(t, events.PurgeExportsJobName)
	if res, err := e.mod.Sweeper.Sweep(t.Context(), p, at); err != nil || res.Count != 1 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	e.stored(t, x) // a dry run deletes nothing
	p.LegalApproved = true
	if res, err := e.mod.Sweeper.Sweep(t.Context(), p, t0.Add(729*day)); err != nil || res.Count != 0 {
		t.Fatalf("sweep before 2 years = %+v, %v", res, err)
	}
	if res, err := e.mod.Sweeper.Sweep(t.Context(), p, at); err != nil || res.Count != 1 {
		t.Fatalf("sweep after 2 years = %+v, %v", res, err)
	}
	if _, err := e.mem.GetExport(t.Context(), x.OrgID, x.EventID, x.ID); !errors.Is(err, events.ErrNotFound) {
		t.Fatalf("the export row survived its retention: %v", err)
	}
}
