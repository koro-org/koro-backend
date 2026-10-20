//go:build integration

package events_test

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"testing"
	"uuid"

	"koro/internal/events"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// pgWorld inserts the parent rows as app_rw.
func pgWorld(d *db.DB) world {
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	w := world{
		org: func(t *testing.T) uuid.UUID {
			id, n := uuid.NewV7(), strconv.FormatInt(seq.Add(1), 10)
			exec(t, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'club', $2, $3)`, id, "Org "+n, "org-"+n)
			return id
		},
		user: func(t *testing.T) uuid.UUID {
			id := uuid.NewV7()
			email := "u" + strconv.FormatInt(seq.Add(1), 10) + "@example.test"
			exec(t, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
				VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, email)
			return id
		},
		university: func(t *testing.T) uuid.UUID {
			org, id, n := uuid.NewV7(), uuid.NewV7(), strconv.FormatInt(seq.Add(1), 10)
			exec(t, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'university', $2, $3)`, org, "Uni "+n, "uni-"+n)
			exec(t, `INSERT INTO universities (id, org_id, yok_code, name, city, type) VALUES ($1, $2, $3, $4, 'Ankara', 'state')`,
				id, org, "T-"+n, "Üniversite "+n)
			return id
		},
	}
	w.file = func(t *testing.T) uuid.UUID {
		id := uuid.NewV7()
		exec(t, `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime) VALUES ($1, 'event_export', 'files', $2, 'text/csv')`,
			id, "event_export/2026/10/"+id.String())
		return id
	}
	w.register = func(t *testing.T, _ events.Store, org, eventID uuid.UUID) {
		exec(t, `INSERT INTO event_registrations (event_id, org_id, user_id, status, source, answers_purge_at)
			VALUES ($1, $2, $3, 'pending', 'web', now() + interval '90 days')`, eventID, org, w.user(t))
	}
	return w
}

func TestStore_FormContract_Gorm(t *testing.T) {
	t.Parallel()
	formStoreContract(t, func(t *testing.T) (events.Store, world) {
		d := testutil.NewDB(t)
		return events.NewGormStore(d.ORM), pgWorld(d)
	})
}

// TestStore_GormSeedMatchesDefaultLintRules: migration 000060 and
// DefaultLintRules (the in-memory seed) are the same 19 rules, and every
// seeded pattern compiles as Go RE2 and does not match the empty text.
func TestStore_GormSeedMatchesDefaultLintRules(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	got, err := events.NewGormStore(d.ORM).ListLintRules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := events.DefaultLintRules()
	if len(got) != len(want) {
		t.Fatalf("seed has %d rules, DefaultLintRules %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || g.Class != w.Class || g.Pattern != w.Pattern || g.DescriptionTR != w.DescriptionTR || g.Active != w.Active {
			t.Fatalf("rule %d:\n db  %+v\n go  %+v", i, g, w)
		}
		re, err := regexp.Compile(g.Pattern)
		if err != nil || re.MatchString("") {
			t.Fatalf("seed rule %s does not compile or matches the empty text: %v", g.ID, err)
		}
	}
}

// TestStore_GormFormVersionImmutable: app_rw can neither update nor delete
// a published version (02 §11.4: no grant, and koro_forbid_mutation).
func TestStore_GormFormVersionImmutable(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	s := events.NewGormStore(d.ORM)
	ctx := t.Context()
	org, by := w.org(t), w.user(t)
	e := row(org, by, "Değişmez", start)
	if _, ok, err := s.InsertEvent(ctx, e); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.InsertForm(ctx, org, events.Form{ID: uuid.NewV7(), EventID: e.ID, DraftSchema: events.JSON(`{"schema_version":1,"fields":[]}`)}); err != nil {
		t.Fatal(err)
	}
	f, err := s.GetForm(ctx, org, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	sc := events.FormSchema{SchemaVersion: 1, Fields: []events.FormField{}}
	v, err := s.InsertFormVersion(ctx, org, events.FormVersion{ID: uuid.NewV7(), FormID: f.ID, Schema: events.JSON(sc.Canonical()),
		SchemaSHA256: sc.SHA256(), LintReport: events.LintReport{}.JSON(), PublishedBy: by, PublishedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE event_form_versions SET version_no = 9 WHERE id = $1`,
		`UPDATE event_form_versions SET schema = '{"schema_version":1,"fields":[]}' WHERE id = $1`,
		`DELETE FROM event_form_versions WHERE id = $1`,
	} {
		if _, err := d.Pool.Exec(ctx, stmt, v.ID); err == nil {
			t.Fatalf("%s succeeded", stmt)
		}
	}
	got, err := s.GetFormVersion(ctx, org, v.ID)
	if err != nil || got.VersionNo != 1 {
		t.Fatalf("version = %+v, %v", got, err)
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (events.Store, world) {
		d := testutil.NewDB(t)
		return events.NewGormStore(d.ORM), pgWorld(d)
	})
}

// TestStore_GormDeleteDraftKeepsPublishedForm: a draft whose form already
// points at a published version (BE-026) cannot be deleted (the versions
// are immutable): 409 events.invalid_state, nothing deleted. A published
// event is kept by the store and by the BEFORE DELETE trigger.
func TestStore_GormDeleteDraftKeepsPublishedForm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	s := events.NewGormStore(d.ORM)
	ctx := t.Context()
	org, by := w.org(t), w.user(t)
	e := row(org, by, "Formlu Taslak", start)
	e.Status, e.PublishedAt = events.StatusDraft, nil
	if _, ok, err := s.InsertEvent(ctx, e); err != nil || !ok {
		t.Fatal(ok, err)
	}
	formID := uuid.NewV7()
	if err := s.InsertForm(ctx, org, events.Form{ID: formID, EventID: e.ID, DraftSchema: events.JSON(`{"schema_version":1,"fields":[]}`)}); err != nil {
		t.Fatal(err)
	}
	version := uuid.NewV7()
	if _, err := d.Pool.Exec(ctx, `INSERT INTO event_form_versions (id, form_id, event_id, version_no, schema, schema_sha256,
		field_count, has_sensitive_fields, lint_report, published_by)
		VALUES ($1, $2, $3, 1, '{"schema_version":1,"fields":[]}', decode(repeat('ab', 32), 'hex'), 0, false,
		'{"blocked":[],"warn_special":[],"review_identifier":[]}', $4)`, version, formID, e.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE event_forms SET current_version_id = $1 WHERE id = $2`, version, formID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasPublishedForm(ctx, org, e.ID); err != nil || !ok {
		t.Fatalf("HasPublishedForm = %v, %v", ok, err)
	}
	if err := d.Tx.WithTx(ctx, func(ctx context.Context) error { return s.DeleteDraft(ctx, org, e.ID) }); !errors.Is(err, events.ErrInvalidState) {
		t.Fatalf("DeleteDraft with a published form = %v", err)
	}
	if _, err := s.GetEvent(ctx, e.ID); err != nil {
		t.Fatalf("the draft is gone: %v", err)
	}
}
