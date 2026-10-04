package events_test

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/platform/problem"
)

func TestStore_FormContract_Mem(t *testing.T) {
	t.Parallel()
	formStoreContract(t, func(*testing.T) (events.Store, world) {
		return events.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// version is a valid version row of form formID (lint report empty).
func version(formID, by uuid.UUID, s events.FormSchema) events.FormVersion {
	return events.FormVersion{
		ID: uuid.NewV7(), FormID: formID, Schema: events.JSON(s.Canonical()), SchemaSHA256: s.SHA256(),
		FieldCount: len(s.Fields), LintReport: events.LintReport{}.JSON(), PublishedBy: by, PublishedAt: t0,
	}
}

// formStoreContract is the form side of the store contract (02 §11.3–11.5,
// BE-026): both stores pass it.
func formStoreContract(t *testing.T, newStore func(t *testing.T) (events.Store, world)) {
	t.Helper()
	// setup is an event of org with its empty form.
	setup := func(t *testing.T, s events.Store, org, by uuid.UUID) (events.Event, events.Form) {
		t.Helper()
		e, ok, err := s.InsertEvent(t.Context(), row(org, by, "Formlu", start))
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := s.InsertForm(t.Context(), org, events.Form{ID: uuid.NewV7(), EventID: e.ID,
			DraftSchema: events.JSON(`{"schema_version":1,"fields":[]}`)}); err != nil {
			t.Fatal(err)
		}
		f, err := s.GetForm(t.Context(), org, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		return e, f
	}
	one := events.FormSchema{SchemaVersion: 1, Fields: []events.FormField{{Key: "motivation", Type: events.FieldLongText,
		Label: events.LocalText{"tr": "Neden?"}, Required: true}}}
	empty := events.FormSchema{SchemaVersion: 1, Fields: []events.FormField{}}

	t.Run("draft: read, lock, replace within the org", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		e, f := setup(t, s, org, by)
		if f.EventID != e.ID || f.OrgID != org || f.CurrentVersionID != nil {
			t.Fatalf("form = %+v", f)
		}
		if _, err := s.GetForm(ctx, other, e.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("GetForm(other org) = %v", err)
		}
		if _, err := s.LockForm(ctx, other, e.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("LockForm(other org) = %v", err)
		}
		if _, err := s.UpdateFormDraft(ctx, other, f.ID, events.JSON(one.Canonical()), by); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("UpdateFormDraft(other org) = %v", err)
		}
		up, err := s.UpdateFormDraft(ctx, org, f.ID, events.JSON(one.Canonical()), by)
		if err != nil || up.UpdatedBy == nil || *up.UpdatedBy != by || up.UpdatedAt.Before(f.UpdatedAt) {
			t.Fatalf("UpdateFormDraft = %+v, %v", up, err)
		}
		got, err := s.LockForm(ctx, org, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		d, err := events.DecodeFormSchema(got.DraftSchema)
		if err != nil || !bytes.Equal(d.Canonical(), one.Canonical()) {
			t.Fatalf("draft = %s, %v", got.DraftSchema, err)
		}
	})

	t.Run("versions: numbered per form, scoped to the org, current only of its form", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		e, f := setup(t, s, org, by)
		_, f2 := setup(t, s, org, by)
		if _, err := s.InsertFormVersion(ctx, other, version(f.ID, by, empty)); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("InsertFormVersion(other org) = %v", err)
		}
		v1, err := s.InsertFormVersion(ctx, org, version(f.ID, by, empty))
		if err != nil || v1.VersionNo != 1 || v1.EventID != e.ID || v1.FormID != f.ID || v1.FieldCount != 0 {
			t.Fatalf("v1 = %+v, %v", v1, err)
		}
		v2, err := s.InsertFormVersion(ctx, org, version(f.ID, by, one))
		if err != nil || v2.VersionNo != 2 || !bytes.Equal(v2.SchemaSHA256, one.SHA256()) || !v2.PublishedAt.Equal(t0) {
			t.Fatalf("v2 = %+v, %v", v2, err)
		}
		// The numbering is per form.
		if w1, err := s.InsertFormVersion(ctx, org, version(f2.ID, by, one)); err != nil || w1.VersionNo != 1 {
			t.Fatalf("other form's v1 = %+v, %v", w1, err)
		}
		if _, err := s.SetCurrentVersion(ctx, org, f2.ID, v2.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("SetCurrentVersion(another form's version) = %v", err)
		}
		if _, err := s.SetCurrentVersion(ctx, other, f.ID, v2.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("SetCurrentVersion(other org) = %v", err)
		}
		cur, err := s.SetCurrentVersion(ctx, org, f.ID, v2.ID)
		if err != nil || cur.CurrentVersionID == nil || *cur.CurrentVersionID != v2.ID {
			t.Fatalf("SetCurrentVersion = %+v, %v", cur, err)
		}
		if ok, err := s.HasPublishedForm(ctx, org, e.ID); err != nil || !ok {
			t.Fatalf("HasPublishedForm = %v, %v", ok, err)
		}
		got, err := s.GetFormVersion(ctx, org, v2.ID)
		if err != nil || got.VersionNo != 2 || !bytes.Equal(got.SchemaSHA256, v2.SchemaSHA256) {
			t.Fatalf("GetFormVersion = %+v, %v", got, err)
		}
		sc, err := events.DecodeFormSchema(got.Schema)
		if err != nil || !bytes.Equal(sc.Canonical(), one.Canonical()) {
			t.Fatalf("stored schema = %s, %v", got.Schema, err)
		}
		if _, err := s.GetFormVersion(ctx, other, v2.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("GetFormVersion(other org) = %v", err)
		}
		list, err := s.ListFormVersions(ctx, org, f.ID)
		if err != nil || len(list) != 2 || list[0].ID != v1.ID || list[1].ID != v2.ID {
			t.Fatalf("ListFormVersions = %+v, %v", list, err)
		}
		if list, err := s.ListFormVersions(ctx, other, f.ID); err != nil || len(list) != 0 {
			t.Fatalf("ListFormVersions(other org) = %+v, %v", list, err)
		}
	})

	t.Run("versions: the lint CHECKs", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t), w.user(t)
		_, f := setup(t, s, org, by)
		hit := events.LintHit{FieldKey: "motivation", RuleID: uuid.NewV7(), DescriptionTR: "Kural"}
		blocked := version(f.ID, by, one)
		blocked.LintReport = events.LintReport{Blocked: []events.LintHit{hit}}.JSON()
		sensitiveNoOverride := version(f.ID, by, one)
		sensitiveNoOverride.LintReport = events.LintReport{WarnSpecial: []events.LintHit{hit}}.JSON()
		sensitiveNoOverride.HasSensitiveFields = true
		flagLie := version(f.ID, by, one)
		flagLie.LintReport = events.LintReport{WarnSpecial: []events.LintHit{hit}}.JSON()
		flagLie.LintOverrideBy, flagLie.LintOverrideReason = &by, new("Gerekçe yeterince uzun.")
		shortReason := version(f.ID, by, one)
		shortReason.LintOverrideBy, shortReason.LintOverrideReason = &by, new("kısa")
		halfOverride := version(f.ID, by, one)
		halfOverride.LintOverrideBy = &by
		for name, v := range map[string]events.FormVersion{
			"blocked hit": blocked, "sensitive without override": sensitiveNoOverride,
			"has_sensitive_fields left false": flagLie, "reason under 10": shortReason, "override without reason": halfOverride,
		} {
			if _, err := s.InsertFormVersion(ctx, org, v); !errors.Is(err, problem.ErrValidation) {
				t.Fatalf("%s: InsertFormVersion = %v", name, err)
			}
		}
		ok := version(f.ID, by, one)
		ok.LintReport = events.LintReport{WarnSpecial: []events.LintHit{hit}}.JSON()
		ok.HasSensitiveFields, ok.LintOverrideBy, ok.LintOverrideReason = true, &by, new("Gerekçe yeterince uzun.")
		got, err := s.InsertFormVersion(ctx, org, ok)
		if err != nil || !got.HasSensitiveFields || got.LintOverrideBy == nil || *got.LintOverrideBy != by || got.VersionNo != 1 {
			t.Fatalf("InsertFormVersion(override) = %+v, %v", got, err)
		}
		rep, err := events.DecodeLintReport(got.LintReport)
		if err != nil || len(rep.WarnSpecial) != 1 || rep.WarnSpecial[0].RuleID != hit.RuleID {
			t.Fatalf("stored report = %+v, %v", rep, err)
		}
	})

	t.Run("registrations: whether an event has one", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		e, _ := setup(t, s, org, by)
		if ok, err := s.HasRegistrations(ctx, org, e.ID); err != nil || ok {
			t.Fatalf("HasRegistrations(none) = %v, %v", ok, err)
		}
		w.register(t, s, org, e.ID)
		if ok, err := s.HasRegistrations(ctx, org, e.ID); err != nil || !ok {
			t.Fatalf("HasRegistrations = %v, %v", ok, err)
		}
		if ok, err := s.HasRegistrations(ctx, other, e.ID); err != nil || ok {
			t.Fatalf("HasRegistrations(other org) = %v, %v", ok, err)
		}
	})

	t.Run("lint rules: the seed, insert, update, never delete", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		by := w.user(t)
		rules, err := s.ListLintRules(ctx)
		if err != nil || len(rules) != len(events.DefaultLintRules()) {
			t.Fatalf("ListLintRules = %d, %v", len(rules), err)
		}
		for i, r := range events.DefaultLintRules() {
			if rules[i].ID != r.ID || rules[i].Class != r.Class || rules[i].Pattern != r.Pattern ||
				rules[i].DescriptionTR != r.DescriptionTR || rules[i].Active != r.Active {
				t.Fatalf("rule %d = %+v, want %+v", i, rules[i], r)
			}
		}
		r, err := s.InsertLintRule(ctx, events.LintRule{ID: uuid.NewV7(), Class: events.LintReviewIdentifier,
			Pattern: `\bogrenci\s*no`, DescriptionTR: "Öğrenci numarası", Active: true, UpdatedBy: &by})
		if err != nil || r.CreatedAt.IsZero() || r.UpdatedBy == nil {
			t.Fatalf("InsertLintRule = %+v, %v", r, err)
		}
		list, _ := s.ListLintRules(ctx)
		if list[len(list)-1].ID != r.ID {
			t.Fatalf("a new review_identifier rule is not last: %+v", list[len(list)-1])
		}
		r.Class, r.Active = events.LintBlocked, false
		up, err := s.UpdateLintRule(ctx, r)
		if err != nil || up.Class != events.LintBlocked || up.Active || up.UpdatedAt.Before(r.UpdatedAt) {
			t.Fatalf("UpdateLintRule = %+v, %v", up, err)
		}
		if got, err := s.GetLintRule(ctx, r.ID); err != nil || got.Class != events.LintBlocked || got.Active {
			t.Fatalf("GetLintRule = %+v, %v", got, err)
		}
		if _, err := s.GetLintRule(ctx, uuid.NewV7()); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("GetLintRule(unknown) = %v", err)
		}
		gone := r
		gone.ID = uuid.NewV7()
		if _, err := s.UpdateLintRule(ctx, gone); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("UpdateLintRule(unknown) = %v", err)
		}
		bad := r
		bad.ID, bad.Class = uuid.NewV7(), "secret"
		if _, err := s.InsertLintRule(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("InsertLintRule(bad class) = %v", err)
		}
	})
}
