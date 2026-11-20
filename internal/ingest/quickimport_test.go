package ingest_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/ingest"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/ratelimit"
)

// fakeFiles is files' Get and Read over a map: the club's poster uploads.
type fakeFiles struct {
	mu    sync.Mutex
	metas map[uuid.UUID]files.FileMeta
	data  map[uuid.UUID][]byte
	reads int
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{metas: map[uuid.UUID]files.FileMeta{}, data: map[uuid.UUID][]byte{}}
}

func (f *fakeFiles) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.metas[id]
	if !ok {
		return files.FileMeta{}, files.ErrNotFound
	}
	return m, nil
}

func (f *fakeFiles) Read(_ context.Context, id uuid.UUID, maxBytes int64) ([]byte, files.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	m, ok := f.metas[id]
	switch {
	case !ok:
		return nil, m, files.ErrNotFound
	case m.ScanStatus != files.ScanClean:
		return nil, m, files.ErrNotClean
	case int64(len(f.data[id])) > maxBytes:
		return nil, m, files.ErrTooLarge
	}
	return f.data[id], m, nil
}

func (f *fakeFiles) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// poster adds a clean, completed event_cover upload of org (a PNG header);
// change adjusts it.
func (f *fakeFiles) poster(org uuid.UUID, change func(*files.FileMeta)) uuid.UUID {
	id := uuid.NewV7()
	body := []byte("\x89PNG\r\n\x1a\nposter")
	m := files.FileMeta{ID: id, Purpose: files.PurposeEventCover, OrgID: &org, MIME: files.MIMEPNG, SizeBytes: int64(len(body)),
		Status: files.StatusActive, ScanStatus: files.ScanClean, AttachedEntityID: uuid.Nil()}
	if change != nil {
		change(&m)
	}
	f.mu.Lock()
	f.metas[id], f.data[id] = m, body
	f.mu.Unlock()
	return id
}

// quickClub opens a club of a new university and returns it with its
// club_admin.
func quickClub(t *testing.T, e *env, code, name string) (uuid.UUID, authz.Actor) {
	t.Helper()
	uni := e.university(t, code, name+" Üniversitesi", strings.ToLower(code)+".example.org")
	club, _, err := e.dir.ImportClub(t.Context(), directory.ImportedClub{UniversityID: uni.ID, Name: name,
		Category: directory.CategoryTechnology, Job: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return club, clubAdmin(club)
}

// limiterCalls is the number of limiter calls whose rule starts with
// ingest_quick_import.
func limiterCalls(e *env) int {
	n := 0
	for _, c := range e.limiter.Calls() {
		if strings.HasPrefix(c, "ingest_quick_import") {
			n++
		}
	}
	return n
}

const quickPost = "YAPAY ZEKÂ ATÖLYESİ\n12 Aralık 2026 18:30\n📍 B Blok Konferans Salonu\nKonuşmacı: Deneme Kişi\n" +
	"Başvuru ve bilgi: kulup@example.org / 0 (212) 000 00 00"

// TestQuickImport_TextIsMaskedAndReadIntoDrafts (BE-211): the club admin's
// pasted post leaves for the extractor masked (no address, phone or named
// speaker), comes back as a draft with createEvent's fields (title, start
// in Europe/Istanbul, place, summary) and a confidence per field, verified
// against the text; nothing is stored (no imported event, no draft) and
// the call is audited without its content.
func TestQuickImport_TextIsMaskedAndReadIntoDrafts(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{})
	ctx := t.Context()
	club, ca := quickClub(t, e, "QI1", "Bilişim Kulübü")

	r, err := e.svc.QuickImport(ctx, ca, club, ingest.QuickImportInput{Text: quickPost})
	if err != nil {
		t.Fatal(err)
	}
	sent := e.ext.drafts[0]
	for _, leaked := range []string{"kulup@example.org", "Deneme", "000 00 00"} {
		if strings.Contains(sent.Text, leaked) {
			t.Errorf("the extractor got %q: %s", leaked, sent.Text)
		}
	}
	if !strings.Contains(sent.Text, "[kişi]") || sent.Club != "Bilişim Kulübü" || len(sent.Images) != 0 {
		t.Fatalf("request = %+v", sent)
	}
	if len(r.Drafts) != 1 || !r.ImagesRead {
		t.Fatalf("result = %+v", r)
	}
	d := r.Drafts[0]
	want := time.Date(2026, 12, 12, 18, 30, 0, 0, istanbulLoc(t))
	if d.Title != "YAPAY ZEKÂ ATÖLYESİ" || d.StartsAt == nil || !d.StartsAt.Equal(want) || d.AllDay ||
		d.Location != "B Blok Konferans Salonu" || d.Online || !d.Verified || d.Category != ingest.CategoryOther {
		t.Fatalf("draft = %+v", d)
	}
	if d.Confidence.Start != 0.9 || d.Confidence.Location != 0.6 || d.Confidence.End != 0 {
		t.Fatalf("confidence = %+v", d.Confidence)
	}
	if strings.Contains(d.Summary, "example.org") || strings.Contains(d.Summary, "Deneme") || d.Summary == "" {
		t.Fatalf("summary = %q", d.Summary)
	}
	if page, err := e.svc.AdminListImportedEvents(ctx, admin(t), ingest.AdminEventQuery{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("imported events = %+v %v", page, err)
	}
	if len(e.drafts.inputs) != 0 {
		t.Fatal("a quick import created an events draft")
	}
	evs := e.audit.Of(authz.IngestQuickImportRun)
	if len(evs) != 1 || evs[0].OrgID == nil || *evs[0].OrgID != club || evs[0].Metadata["drafts"] != 1 ||
		evs[0].Metadata["text_chars"] == nil {
		t.Fatalf("audit = %+v", evs)
	}
	for k, v := range evs[0].Metadata {
		if s, ok := v.(string); ok && strings.Contains(s, "ATÖLYE") {
			t.Fatalf("audit metadata %s carries the post", k)
		}
	}
}

// TestQuickImport_UnverifiedDraftsAndPosterDates (BE-211): with text only,
// a draft whose evidence is not a quote of the text is dropped (invented)
// and a date whose day the quote does not hold is left out (start
// confidence 0); with a poster the same answer is kept unverified, the
// poster's date with it.
func TestQuickImport_UnverifiedDraftsAndPosterDates(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{QuickImportImages: true})
	ctx := t.Context()
	club, ca := quickClub(t, e, "QI2", "Kodlama Kulübü")
	text := "Kodlama Gecesi bu cuma 20:00'de, herkes davetli!"
	e.ext.draftScript = func(ingest.DraftRequest) (ingest.DraftExtraction, error) {
		return ingest.DraftExtraction{ImagesRead: true, Drafts: []ingest.DraftCandidate{
			{Title: "Hackathon", StartDate: "2026-12-05", YearStated: true, Evidence: "Hackathon 5 Aralık 2026",
				Confidence: ingest.DraftConfidence{Title: 0.9, Start: 0.9}},
			{Title: "Kodlama Gecesi", StartDate: "2026-11-13", StartTime: "20:00", YearStated: true,
				Evidence: "Kodlama Gecesi bu cuma 20:00'de", Confidence: ingest.DraftConfidence{Title: 0.9, Start: 0.8}},
		}}, nil
	}
	r, err := e.svc.QuickImport(ctx, ca, club, ingest.QuickImportInput{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Drafts) != 1 || r.Drafts[0].Title != "Kodlama Gecesi" || !r.Drafts[0].Verified ||
		r.Drafts[0].StartsAt != nil || r.Drafts[0].Confidence.Start != 0 {
		t.Fatalf("text only = %+v", r.Drafts)
	}

	poster := e.files.poster(club, nil)
	r, err = e.svc.QuickImport(ctx, ca, club, ingest.QuickImportInput{Text: text, FileIDs: []uuid.UUID{poster}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Drafts) != 2 || r.Drafts[0].Verified || r.Drafts[0].StartsAt == nil || r.Drafts[0].Confidence.Start != 0.9 {
		t.Fatalf("with a poster = %+v", r.Drafts)
	}
	if img := e.ext.drafts[1].Images; len(img) != 1 || img[0].MediaType != files.MIMEPNG || len(img[0].Data) == 0 {
		t.Fatalf("images sent = %+v", img)
	}
}

// TestQuickImport_PostersOnlyWhereAllowed (BE-211): where reading posters
// is off (prod until the lawyer's review) a poster is 422
// ingest.quick_import_images_off before anything is spent or read; where
// it is on, the poster's bytes go to the extractor, and the development
// extractor says it read no image (images_read false).
func TestQuickImport_PostersOnlyWhereAllowed(t *testing.T) {
	t.Parallel()
	off := newEnv(t, ingest.Config{})
	club, ca := quickClub(t, off, "QI3", "Fotoğraf Kulübü")
	poster := off.files.poster(club, nil)
	_, err := off.svc.QuickImport(t.Context(), ca, club, ingest.QuickImportInput{Text: quickPost, FileIDs: []uuid.UUID{poster}})
	problemtest.WantErr(t, err, ingest.ErrQuickImportImagesOff)
	if limiterCalls(off) != 0 || off.files.readCount() != 0 || off.ext.count() != 0 {
		t.Fatal("a refused poster spent the budget, was read or reached the extractor")
	}

	on := newEnv(t, ingest.Config{QuickImportImages: true})
	club, ca = quickClub(t, on, "QI4", "Fotoğraf Kulübü")
	poster = on.files.poster(club, nil)
	r, err := on.svc.QuickImport(t.Context(), ca, club, ingest.QuickImportInput{FileIDs: []uuid.UUID{poster}})
	if err != nil {
		t.Fatal(err)
	}
	if r.ImagesRead || len(r.Drafts) != 0 || on.files.readCount() != 1 || len(on.ext.drafts[0].Images) != 1 {
		t.Fatalf("fake extractor with a poster = %+v (reads %d)", r, on.files.readCount())
	}
}

// TestQuickImport_PosterChecks (BE-211): a poster must be a completed,
// scanned event_cover upload of this club (06 H-38: the club comes from
// the file row), at most three, each once, small enough; every refusal
// comes before the budget is spent.
func TestQuickImport_PosterChecks(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{QuickImportImages: true})
	ctx := t.Context()
	club, ca := quickClub(t, e, "QI5", "Afiş Kulübü")
	other, _ := quickClub(t, e, "QI6", "Başka Kulüp")
	good := e.files.poster(club, nil)
	for _, tc := range []struct {
		name string
		ids  []uuid.UUID
		want error // nil: a 422 on a field
		loc  string
	}{
		{"unknown", []uuid.UUID{uuid.NewV7()}, nil, "body.file_ids[0]"},
		{"another club's", []uuid.UUID{e.files.poster(other, nil)}, nil, "body.file_ids[0]"},
		{"another purpose", []uuid.UUID{e.files.poster(club, func(m *files.FileMeta) { m.Purpose = files.PurposePostMedia })},
			nil, "body.file_ids[0]"},
		{"twice", []uuid.UUID{good, good}, nil, "body.file_ids[1]"},
		{"four", []uuid.UUID{good, e.files.poster(club, nil), e.files.poster(club, nil), e.files.poster(club, nil)},
			nil, "body.file_ids"},
		{"not completed", []uuid.UUID{e.files.poster(club, func(m *files.FileMeta) { m.Status = files.StatusUploading })},
			nil, "body.file_ids[0]"},
		{"too large", []uuid.UUID{e.files.poster(club, func(m *files.FileMeta) { m.SizeBytes = 6 << 20 })}, nil,
			"body.file_ids[0]"},
		{"scan pending", []uuid.UUID{e.files.poster(club, func(m *files.FileMeta) { m.ScanStatus = files.ScanPending })},
			files.ErrNotClean, ""},
		{"infected", []uuid.UUID{e.files.poster(club, func(m *files.FileMeta) { m.ScanStatus = files.ScanInfected })},
			files.ErrRejected, ""},
		{"retired", []uuid.UUID{e.files.poster(club, func(m *files.FileMeta) { m.Retired = true })}, files.ErrRejected, ""},
	} {
		_, err := e.svc.QuickImport(ctx, ca, club, ingest.QuickImportInput{FileIDs: tc.ids})
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
			}
			continue
		}
		pe, ok := errors.AsType[*problem.Error](err)
		if !ok || len(pe.Fields()) != 1 || pe.Fields()[0].Location != tc.loc {
			t.Errorf("%s: err = %v, want a 422 on %s", tc.name, err, tc.loc)
		}
	}
	if limiterCalls(e) != 0 || e.files.readCount() != 0 {
		t.Fatalf("refused posters spent %d limiter calls, %d reads", limiterCalls(e), e.files.readCount())
	}
}

// TestQuickImport_AuthzClubStateAndInput (BE-211): the club's club_admin
// only (a member 403, another club's admin 404, a platform admin 403),
// a club only (another kind of org 404), an active club only (suspended:
// orgs.org_suspended); an empty body or text with control characters is
// 422; nothing of these reaches the budget or the extractor.
func TestQuickImport_AuthzClubStateAndInput(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{})
	ctx := t.Context()
	club, ca := quickClub(t, e, "QI7", "Satranç Kulübü")
	other, otherAdmin := quickClub(t, e, "QI8", "Tiyatro Kulübü")
	in := ingest.QuickImportInput{Text: quickPost}
	call := func(a authz.Actor, org uuid.UUID, in ingest.QuickImportInput) error {
		_, err := e.svc.QuickImport(ctx, a, org, in)
		return err
	}
	problemtest.WantErr(t, call(member(club), club, in), authz.ErrForbidden)
	problemtest.WantErr(t, call(otherAdmin, club, in), authz.ErrNotFound)
	problemtest.WantErr(t, call(admin(t), club, in), authz.ErrForbidden)
	problemtest.WantErr(t, call(ca, uuid.NewV7(), in), ingest.ErrNotFound)
	uni, err := e.dirMem.UniversityByYokCode(ctx, "QI7")
	if err != nil {
		t.Fatal(err)
	}
	if o, err := e.orgsMem.GetOrg(ctx, uni.OrgID); err != nil || o.Kind != orgs.KindUniversity {
		t.Fatalf("university org = %+v %v", o, err)
	}
	problemtest.WantErr(t, call(ca, uni.OrgID, in), ingest.ErrNotFound) // not a club
	for _, bad := range []string{"", "   \n ", "Atölye\u202e 12 Aralık"} {
		if _, ok := errors.AsType[*problem.Error](call(ca, club, ingest.QuickImportInput{Text: bad})); !ok {
			t.Errorf("text %q accepted", bad)
		}
	}
	problemtest.WantErr(t, call(ca, club, ingest.QuickImportInput{Text: strings.Repeat("a", 5001)}), problem.ErrValidation)

	o, err := e.orgsMem.GetOrg(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	o.Status = orgs.StatusSuspended
	if _, err := e.orgsMem.UpdateOrg(ctx, o); err != nil {
		t.Fatal(err)
	}
	problemtest.WantErr(t, call(otherAdmin, other, in), orgs.ErrOrgSuspended)
	if limiterCalls(e) != 0 || e.ext.count() != 0 {
		t.Fatalf("refused calls spent %d limiter calls, %d extractions", limiterCalls(e), e.ext.count())
	}
}

// TestQuickImport_ClubLimitsAndDailyBudget (BE-211): a club gets its
// hourly limit (429 with Retry-After, then again after the hour), every
// club shares the platform's daily budget, and an unavailable limiter
// fails closed (503).
func TestQuickImport_ClubLimitsAndDailyBudget(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{QuickImportOrgHourly: 2, QuickImportOrgDaily: 10, QuickImportDailyBudget: 3})
	ctx := t.Context()
	club, ca := quickClub(t, e, "QI9", "Müzik Kulübü")
	other, otherAdmin := quickClub(t, e, "QI10", "Dans Kulübü")
	in := ingest.QuickImportInput{Text: quickPost}
	for range 2 {
		if _, err := e.svc.QuickImport(ctx, ca, club, in); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.svc.QuickImport(ctx, ca, club, in)
	problemtest.WantErr(t, err, problem.ErrRateLimited)
	if pe, _ := errors.AsType[*problem.Error](err); pe == nil || pe.RetryAfter() <= 0 || pe.RetryAfter() > time.Hour {
		t.Fatalf("retry after = %v", err)
	}
	e.clk.Advance(time.Hour)
	if _, err := e.svc.QuickImport(ctx, otherAdmin, other, in); err != nil {
		t.Fatalf("another club within the daily budget: %v", err)
	}
	// The daily budget (3) is spent: the club's two calls and the other club's one (a refused call
	// spends nothing further down the chain).
	if _, err := e.svc.QuickImport(ctx, ca, club, in); !errors.Is(err, problem.ErrRateLimited) {
		t.Fatalf("budget spent = %v", err)
	}
	e.clk.Advance(24 * time.Hour)
	e.limiter.Fail(ratelimit.ErrUnavailable)
	problemtest.WantErr(t, func() error { _, err := e.svc.QuickImport(ctx, ca, club, in); return err }(), problem.ErrUnavailable)
	if n := counter(e, "quick_imports", "rate_limited"); n != 3 {
		t.Fatalf("rate_limited = %d, want 3", n)
	}
}

// TestQuickImport_ExtractorRefusalAndFailure (BE-211): a model that
// declines gives an empty preview (the club types the event in); an
// extractor that does not answer is 503 and still audited (a poster may
// have left); both are counted, tokens too.
func TestQuickImport_ExtractorRefusalAndFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t, ingest.Config{})
	ctx := t.Context()
	club, ca := quickClub(t, e, "QI11", "Bilim Kulübü")
	in := ingest.QuickImportInput{Text: quickPost}
	e.ext.draftScript = func(ingest.DraftRequest) (ingest.DraftExtraction, error) {
		return ingest.DraftExtraction{InputTokens: 900, OutputTokens: 20}, ingest.ErrExtractRefused
	}
	r, err := e.svc.QuickImport(ctx, ca, club, in)
	if err != nil || len(r.Drafts) != 0 {
		t.Fatalf("refusal = %+v %v", r, err)
	}
	e.ext.draftScript = func(ingest.DraftRequest) (ingest.DraftExtraction, error) {
		return ingest.DraftExtraction{}, errors.New("ingest: extractor API status 529")
	}
	problemtest.WantErr(t, func() error { _, err := e.svc.QuickImport(ctx, ca, club, in); return err }(), problem.ErrUnavailable)
	if n := len(e.audit.OfOutcome(authz.IngestQuickImportRun, audit.Failure)); n != 1 {
		t.Fatalf("failed quick imports audited = %d, want 1 (a poster may have left)", n)
	}
	if counter(e, "quick_imports", "unreadable") != 1 || counter(e, "quick_imports", "failed") != 1 ||
		counter(e, "tokens", "quick_import_input") != 900 || counter(e, "tokens", "input") != 0 {
		t.Fatalf("counters = %+v", e.mod.Counters())
	}
}

func counter(e *env, name, label string) int64 {
	for _, c := range e.mod.Counters() {
		if c.Name == name && c.Label == label {
			return c.Value
		}
	}
	return -1
}

func istanbulLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
