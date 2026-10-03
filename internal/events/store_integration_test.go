//go:build integration

package events_test

import (
	"context"
	"errors"
	"os"
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
	return world{
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
