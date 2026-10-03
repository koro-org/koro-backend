package kvkk_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/kvkk"
)

// zipOf reads the export of request id: file names (zip order) and their
// decoded JSON.
func zipOf(t *testing.T, e *env, id uuid.UUID) ([]string, map[string]map[string]any) {
	t.Helper()
	d, err := e.store.Get(t.Context(), id)
	if err != nil || d.ExportFileID == nil {
		t.Fatalf("request %s has no export file (%v)", id, err)
	}
	raw := e.zips.of(*d.ExportFileID)
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	docs := map[string]map[string]any{}
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		docs[f.Name] = m
	}
	return names, docs
}

// TestExport_ZipContainsAllRegisteredProviders: an export request's zip
// has a manifest and one section per registered provider (kvkk's own
// included), the export file is attached to the request, the subject is
// mailed once even when dsr.completed is delivered twice.
func TestExport_ZipContainsAllRegisteredProviders(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, student := t.Context(), actor(t, "student")
	d, err := e.svc.CreateMine(ctx, student, kvkk.MyRequest{Type: kvkk.TypeExport})
	if err != nil {
		t.Fatal(err)
	}
	e.completeAs(t, d.ID)
	for range 2 { // at-least-once delivery
		if _, err := e.deliver(t, 0); err != nil {
			t.Fatal(err)
		}
	}
	names, docs := zipOf(t, e, d.ID)
	want := []string{"manifest.json"}
	for _, p := range e.providers {
		want = append(want, p.Name()+".json")
	}
	slices.Sort(want[1:])
	if !slices.Equal(names, want) {
		t.Fatalf("zip = %v, want %v", names, want)
	}
	m := docs["manifest.json"]
	if m["format"] != kvkk.ExportFormat || m["dsr_id"] != d.ID.String() || len(m["sections"].([]any)) != len(e.providers) {
		t.Fatalf("manifest = %v", m)
	}
	if items := docs["kvkk.json"]["items"].([]any); len(items) != 1 {
		t.Fatalf("kvkk section = %v", docs["kvkk.json"])
	}
	got, _ := e.store.Get(ctx, d.ID)
	meta, err := e.files.Get(ctx, *got.ExportFileID)
	if err != nil || meta.AttachedModule != "kvkk" || meta.AttachedEntityID != d.ID || meta.Purpose != files.PurposeDSRExport ||
		meta.OwnerUserID == nil || *meta.OwnerUserID != student.UserID {
		t.Fatalf("export file = %+v, %v", meta, err)
	}
	if mails := e.mail.of("kvkk.dsr_completed"); len(mails) != 1 || mails[0].Data["outcome"] != kvkk.OutcomeExport {
		t.Fatalf("subject mails = %+v", mails)
	}
	// Exactly one export per request.
	if n := len(e.zips.content); n != 1 {
		t.Fatalf("%d export files built", n)
	}
}

// TestExport_DownloadAudited_PresignTTL: the download link lives at most 5
// minutes and every issue is audited; after 7 days the purge job deletes
// the file and the link is gone (410).
func TestExport_DownloadAudited_PresignTTL(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, student, dpo := t.Context(), actor(t, "student"), actor(t, "dpo")
	d, err := e.svc.CreateMine(ctx, student, kvkk.MyRequest{Type: kvkk.TypeAccess})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.DownloadMine(ctx, student, d.ID)
	wantErr(t, err, kvkk.ErrExportNotReady)
	e.completeAs(t, d.ID)
	if _, err := e.deliver(t, 0); err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now()
	dl, err := e.svc.DownloadMine(ctx, student, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ttl := dl.ExpiresAt.Sub(now); ttl <= 0 || ttl > 5*time.Minute || dl.URL == "" || kvkk.DownloadTTL != 5*time.Minute {
		t.Fatalf("download = %+v (ttl %s)", dl, dl.ExpiresAt.Sub(now))
	}
	if _, err := e.svc.AdminDownload(ctx, dpo, d.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.DownloadMine(ctx, actor(t, "student_b"), d.ID)
	wantErr(t, err, kvkk.ErrNotFound)
	_, err = e.svc.AdminDownload(ctx, actor(t, "platform_admin"), d.ID)
	wantErr(t, err, authz.ErrForbidden)
	audits := e.audit.of(string(authz.KvkkDsrExportDownload))
	if len(audits) != 2 || *audits[0].ActorUserID != student.UserID || *audits[1].ActorUserID != dpo.UserID ||
		audits[0].Metadata["ttl_seconds"] != 300 || audits[1].Metadata["subject_user_id"] != student.UserID.String() {
		t.Fatalf("download audit = %+v", audits)
	}

	// Day 6: still there. Day 7: purged, never deferred (T-83).
	purge := e.job(t, kvkk.PurgeExportsJobName)
	if purge.Cron != "27 * * * *" || purge.Queue != "low" {
		t.Fatalf("purge job = %s %s", purge.Cron, purge.Queue)
	}
	e.clk.Advance(6 * 24 * time.Hour)
	if err := purge.Handle(workerCtx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DownloadMine(ctx, student, d.ID); err != nil {
		t.Fatalf("day 6 download: %v", err)
	}
	cur, _ := e.store.Get(ctx, d.ID)
	fileID := *cur.ExportFileID
	e.clk.Advance(24 * time.Hour)
	_, err = e.svc.DownloadMine(ctx, student, d.ID) // purge not run yet: still refused
	wantErr(t, err, kvkk.ErrExportExpired)
	if err := purge.Handle(workerCtx(t)); err != nil {
		t.Fatal(err)
	}
	cur, _ = e.store.Get(ctx, d.ID)
	if cur.ExportFileID != nil {
		t.Fatal("export reference kept after the purge")
	}
	if _, err := e.files.Get(ctx, fileID); err == nil {
		t.Fatal("export file kept after the purge")
	}
	_, err = e.svc.DownloadMine(ctx, student, d.ID)
	wantErr(t, err, kvkk.ErrExportExpired)
	if sweep := e.audit.of(string(authz.KvkkRetentionSweep)); len(sweep) != 1 || sweep[0].SystemJob != "kvkk.purge_exports" ||
		sweep[0].Metadata["policy_code"] != "dsr_export_file" {
		t.Fatalf("purge audit = %+v", sweep)
	}
	// A redelivered dsr.completed does not rebuild the purged export.
	if _, err := e.deliver(t, 0); err != nil {
		t.Fatal(err)
	}
	if cur, _ = e.store.Get(ctx, d.ID); cur.ExportFileID != nil {
		t.Fatal("a redelivered event rebuilt a purged export")
	}
}

// TestDSRDeadlines_WarnsAt7Days: the 09:00 job mails the DPO the open
// requests with ≤ 7 days left (overdue included), nothing else.
func TestDSRDeadlines_WarnsAt7Days(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	j := e.job(t, kvkk.DeadlinesJobName)
	if j.Cron != "0 9 * * *" || j.Queue != "default" || j.Unique <= 0 {
		t.Fatalf("deadline job = %+v", j)
	}
	mk := func(persona string, typ kvkk.Type) kvkk.DSR {
		d, err := e.svc.CreateMine(ctx, actor(t, persona), kvkk.MyRequest{Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	overdue := mk("student", kvkk.TypeAccess)
	e.clk.Advance(8 * 24 * time.Hour)
	soon := mk("student", kvkk.TypeInfo) // received 8 days after the first
	e.clk.Advance(2 * 24 * time.Hour)
	later := mk("student_b", kvkk.TypeAccess) // 10 days after the first
	done := mk("club_member", kvkk.TypeInfo)
	e.completeAs(t, done.ID)
	// Now the first is 31 days old (overdue), the second 23 days (7 days
	// left), the third 21 days (9 days left).
	e.clk.Advance(21 * 24 * time.Hour)
	if err := j.Handle(workerCtx(t)); err != nil {
		t.Fatal(err)
	}
	mails := e.mail.of("kvkk.dsr_deadlines")
	if len(mails) != 1 || mails[0].ToEmail != dpoMailbox {
		t.Fatalf("deadline mails = %+v", mails)
	}
	var ids []string
	for _, it := range mails[0].Data["items"].([]map[string]any) {
		ids = append(ids, it["dsr_id"].(string))
	}
	if !slices.Equal(ids, []string{overdue.ID.String(), soon.ID.String()}) || mails[0].Data["overdue"] != 1 {
		t.Fatalf("digest = %v (overdue %v), want the overdue and the 7-day request only", ids, mails[0].Data["overdue"])
	}
	if slices.Contains(ids, later.ID.String()) || slices.Contains(ids, done.ID.String()) {
		t.Fatal("digest lists a request with more than 7 days left or a closed one")
	}
	for _, v := range mails[0].Data {
		if s, ok := v.(string); ok && strings.Contains(s, "@") {
			t.Fatal("the digest carries an address")
		}
	}
	// Same day again: one digest per day.
	if err := j.Handle(workerCtx(t)); err != nil {
		t.Fatal(err)
	}
	if len(e.mail.of("kvkk.dsr_deadlines")) != 1 {
		t.Fatal("second digest on the same day")
	}
}
