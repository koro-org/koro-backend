package events_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/platform/idempotency"
)

// formJSON is a schema of the given fields (JSON objects).
func formJSON(fields ...string) []byte {
	return []byte(`{"schema_version":1,"fields":[` + strings.Join(fields, ",") + `]}`)
}

// textField is an optional short_text field.
func textField(key, label string) string {
	return fmt.Sprintf(`{"key":%q,"type":"short_text","label":{"tr":%q},"required":false}`, key, label)
}

const motivationField = `{"key":"motivation","type":"long_text","label":{"tr":"Neden katılmak istiyorsun?"},"required":true,
	"validation":{"max_length":800},"purpose":{"tr":"Başvuruları değerlendirmek için"}}`

const tshirtField = `{"key":"tshirt","type":"single_select","label":{"tr":"Tişört bedeni"},"required":false,
	"options":[{"value":"s","label":{"tr":"S"}},{"value":"m","label":{"tr":"M"}}]}`

const sponsorField = `{"key":"share_sponsor","type":"consent","required":false,"consent_purpose":"event_sponsor_share"}`

const allergyField = `{"key":"allergy","type":"short_text","label":{"tr":"Gıda alerjiniz var mı?"},"required":false}`

const reason = "Etkinlikte yemek verilecek; alerji bilgisi güvenlik için gerekli."

// formEvent is a new draft event of club_a (with its empty form draft).
func (e *env) formEvent(t *testing.T) events.EventView {
	t.Helper()
	return e.create(t, "club_admin", "club_a", input("Form Etkinliği "+uuid.NewV7().String()[:8]))
}

// putDraft replaces the draft with the current ETag.
func (e *env) putDraft(t *testing.T, actor string, id uuid.UUID, schema []byte) events.FormDraftView {
	t.Helper()
	d, err := e.svc.GetFormDraft(t.Context(), persona(t, actor), id)
	if err != nil {
		t.Fatalf("GetFormDraft: %v", err)
	}
	v, err := e.svc.PutFormDraft(t.Context(), persona(t, actor), id, d.ETag, schema)
	if err != nil {
		t.Fatalf("PutFormDraft: %v %v", err, fieldsOf(err))
	}
	return v
}

// publishForm publishes the current draft as club_admin.
func (e *env) publishForm(t *testing.T, id uuid.UUID, why string) events.FormVersionView {
	t.Helper()
	v, err := e.svc.PublishForm(t.Context(), persona(t, "club_admin"), id, why)
	if err != nil {
		t.Fatalf("PublishForm: %v %v", err, fieldsOf(err))
	}
	return v
}

func wantField(t *testing.T, err error, status int, code, loc string) {
	t.Helper()
	wantCode(t, err, status, code)
	if loc == "" {
		return
	}
	for _, f := range fieldsOf(err) {
		if f.Location == loc {
			return
		}
	}
	t.Fatalf("no field error at %s: %v", loc, fieldsOf(err))
}

// TestFormSchema_MoreThan20Fields_422: 02 §11.4 field_count ≤ 20.
func TestFormSchema_MoreThan20Fields_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	fields := make([]string, 21)
	for i := range fields {
		fields[i] = textField("field_"+strconv.Itoa(i), "Alan "+strconv.Itoa(i))
	}
	d, err := e.svc.GetFormDraft(t.Context(), persona(t, "club_admin"), ev.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.PutFormDraft(t.Context(), persona(t, "club_admin"), ev.Event.ID, d.ETag, formJSON(fields...))
	wantField(t, err, 422, "validation_failed", "body.draft_schema.fields")
	// Exactly 20 is fine.
	v := e.putDraft(t, "club_admin", ev.Event.ID, formJSON(fields[:20]...))
	if len(v.Draft.Fields) != events.MaxFormFields {
		t.Fatalf("fields = %d", len(v.Draft.Fields))
	}
}

// TestFormSchema_InvalidKey_422: keys are ^[a-z][a-z0-9_]{1,40}$, unique and
// never a JavaScript internal name (R-003 §2.5.1).
func TestFormSchema_InvalidKey_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	d, err := e.svc.GetFormDraft(t.Context(), persona(t, "club_admin"), ev.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Motivation", "1abc", "a", "a-b", "ad soyad", "constructor", "__proto__", "çağrı",
		"a" + strings.Repeat("b", 41)} {
		_, err := e.svc.PutFormDraft(t.Context(), persona(t, "club_admin"), ev.Event.ID, d.ETag, formJSON(textField(key, "Ad")))
		wantField(t, err, 422, "validation_failed", "body.draft_schema.fields[0].key")
	}
	// The same key twice.
	_, err = e.svc.PutFormDraft(t.Context(), persona(t, "club_admin"), ev.Event.ID, d.ETag,
		formJSON(textField("name", "Ad"), textField("name", "Soyad")))
	wantField(t, err, 422, "validation_failed", "body.draft_schema.fields[1].key")
	// Unknown properties, types and rules a type does not take.
	for _, tc := range []struct{ raw, loc string }{
		{`{"schema_version":1,"fields":[],"extra":1}`, "body.draft_schema"},
		{`{"schema_version":2,"fields":[]}`, "body.draft_schema.schema_version"},
		{string(formJSON(`{"key":"ad","type":"signature","label":{"tr":"İmza"},"required":false}`)), "body.draft_schema.fields[0].type"},
		{string(formJSON(`{"key":"ad","type":"checkbox","label":{"tr":"Onay"},"required":false,"validation":{"max_length":3}}`)),
			"body.draft_schema.fields[0].validation.max_length"},
		{string(formJSON(`{"key":"ad","type":"short_text","required":false}`)), "body.draft_schema.fields[0].label"},
		{string(formJSON(`{"key":"ad","type":"short_text","label":{"de":"Name"},"required":false}`)), "body.draft_schema.fields[0].label"},
		{string(formJSON(`{"key":"ad","type":"single_select","label":{"tr":"Seç"},"required":false,"options":[]}`)),
			"body.draft_schema.fields[0].options"},
		{string(formJSON(`{"key":"ad","type":"short_text","label":{"tr":"Ad"},"required":false,"validation":{"pattern":"("}}`)),
			"body.draft_schema.fields[0].validation.pattern"},
		{string(formJSON(`{"key":"ok","type":"consent","required":true,"consent_purpose":"event_sponsor_share"}`)),
			"body.draft_schema.fields[0].required"},
		{string(formJSON(`{"key":"ok","type":"consent","required":false,"consent_purpose":"commercial_email"}`)),
			"body.draft_schema.fields[0].consent_purpose"},
		{`[1,2]`, "body.draft_schema"},
	} {
		_, err := e.svc.PutFormDraft(t.Context(), persona(t, "club_admin"), ev.Event.ID, d.ETag, []byte(tc.raw))
		wantField(t, err, 422, "validation_failed", tc.loc)
	}
	// Nothing was stored.
	if again, err := e.svc.GetFormDraft(t.Context(), persona(t, "club_admin"), ev.Event.ID); err != nil ||
		again.ETag != d.ETag || len(again.Draft.Fields) != 0 {
		t.Fatalf("draft changed: %+v %v", again.Draft, err)
	}
}

// TestPutDraft_StaleETag_412: putEventFormDraft needs the draft's ETag
// (04 §1.13): missing → 428, stale → 412.
func TestPutDraft_StaleETag_412(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	ctx, a := t.Context(), persona(t, "club_admin")
	d, err := e.svc.GetFormDraft(ctx, a, ev.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.PutFormDraft(ctx, a, ev.Event.ID, "", formJSON(motivationField))
	wantCode(t, err, 428, "precondition_required")
	_, err = e.svc.PutFormDraft(ctx, a, ev.Event.ID, `"0"`, formJSON(motivationField))
	wantCode(t, err, 412, "precondition_failed")
	v, err := e.svc.PutFormDraft(ctx, a, ev.Event.ID, d.ETag, formJSON(motivationField))
	if err != nil || v.ETag == d.ETag || len(v.Draft.Fields) != 1 {
		t.Fatalf("PutFormDraft = %+v, %v", v, err)
	}
	// The ETag read before the save is stale now: a lost update is refused.
	_, err = e.svc.PutFormDraft(ctx, a, ev.Event.ID, d.ETag, formJSON(tshirtField))
	wantCode(t, err, 412, "precondition_failed")
	if got, _ := e.svc.GetFormDraft(ctx, a, ev.Event.ID); got.Draft.Fields[0].Key != "motivation" || got.ETag != v.ETag {
		t.Fatalf("draft = %+v", got.Draft)
	}
	if n := len(e.audit.of(authz.EventsFormManage)); n != 1 {
		t.Fatalf("audit records = %d", n)
	}
}

// TestPublishForm_BlockedField_422NoOverride: a blocked field (a national
// ID number) is never published, with or without a reason (02 §11.4; the
// DB CHECK says the same). The key alone is enough to be caught.
func TestPublishForm_BlockedField_422NoOverride(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, field := range []string{
		textField("kimlik", "T.C. Kimlik Numaranız"),
		textField("tckn", "Bilgi"),
		textField("tc_kimlik_no", "Bilgi"),
		textField("kart", "KREDİ KARTI NUMARASI"),
		textField("info", "TC"+"\u200d"+"KN"),
	} {
		ev := e.formEvent(t)
		d := e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, field))
		if len(d.Lint.Blocked) == 0 || d.Lint.Blocked[0].FieldKey == "motivation" {
			t.Fatalf("%s: lint preview = %+v", field, d.Lint)
		}
		for _, why := range []string{"", reason} {
			_, err := e.svc.PublishForm(t.Context(), persona(t, "club_admin"), ev.Event.ID, why)
			wantField(t, err, 422, "events.form_blocked_field", "draft_schema.fields[1]")
		}
		if d, _ := e.svc.GetFormDraft(t.Context(), persona(t, "club_admin"), ev.Event.ID); d.CurrentVersionNo != nil {
			t.Fatalf("a version was published: %v", *d.CurrentVersionNo)
		}
		if _, err := e.svc.GetForm(t.Context(), authz.Anonymous(), ev.Event.ID, consent.LocaleTR); err == nil {
			t.Fatal("the form is readable")
		}
	}
}

// TestPublishForm_WarnSpecialWithoutReason_422: a warn_special field
// (health) needs lint_override_reason; a too short one is invalid.
func TestPublishForm_WarnSpecialWithoutReason_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	d := e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, allergyField))
	if len(d.Lint.WarnSpecial) != 1 || d.Lint.WarnSpecial[0].FieldKey != "allergy" || len(d.Lint.Blocked) != 0 {
		t.Fatalf("lint = %+v", d.Lint)
	}
	_, err := e.svc.PublishForm(t.Context(), persona(t, "club_admin"), ev.Event.ID, "")
	wantField(t, err, 422, "events.lint_override_required", "body.lint_override_reason")
	_, err = e.svc.PublishForm(t.Context(), persona(t, "club_admin"), ev.Event.ID, "gerekli")
	wantField(t, err, 422, "validation_failed", "body.lint_override_reason")
	// review_identifier (a passport number) needs the reason too.
	ev2 := e.formEvent(t)
	e.putDraft(t, "club_admin", ev2.Event.ID, formJSON(textField("passport", "Pasaport numarası")))
	_, err = e.svc.PublishForm(t.Context(), persona(t, "club_admin"), ev2.Event.ID, "")
	wantCode(t, err, 422, "events.lint_override_required")
}

// TestPublishForm_WarnSpecialWithReason_SetsOverride: with a reason the
// version is published with has_sensitive_fields, the override columns
// and the reason in the audit record.
func TestPublishForm_WarnSpecialWithReason_SetsOverride(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, allergyField))
	v := e.publishForm(t, ev.Event.ID, "  "+reason+"  ")
	got := v.Version
	admin := persona(t, "club_admin").UserID
	if !got.HasSensitiveFields || got.LintOverrideBy == nil || *got.LintOverrideBy != admin ||
		got.LintOverrideReason == nil || *got.LintOverrideReason != reason || got.VersionNo != 1 || got.FieldCount != 2 {
		t.Fatalf("version = %+v", got)
	}
	rep, err := events.DecodeLintReport(got.LintReport)
	if err != nil || !slices.Equal(rep.SensitiveKeys(), []string{"allergy"}) || len(rep.Blocked) != 0 {
		t.Fatalf("stored report = %+v, %v", rep, err)
	}
	logs := e.audit.of(authz.EventsFormPublish)
	if len(logs) != 1 || logs[0].Reason != reason || logs[0].Metadata["override"] != true ||
		logs[0].Metadata["has_sensitive_fields"] != true {
		t.Fatalf("audit = %+v", logs)
	}
	// Without a sensitive field the reason is not stored.
	ev2 := e.formEvent(t)
	e.putDraft(t, "club_admin", ev2.Event.ID, formJSON(motivationField))
	plain := e.publishForm(t, ev2.Event.ID, reason)
	if plain.Version.LintOverrideBy != nil || plain.Version.LintOverrideReason != nil || plain.Version.HasSensitiveFields {
		t.Fatalf("plain version = %+v", plain.Version)
	}
}

// TestPublishForm_IncrementsVersionAndPointsCurrent: every publication is
// version_no + 1 and becomes the current form; publishing the same schema
// again is 409.
func TestPublishForm_IncrementsVersionAndPointsCurrent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	ctx := t.Context()
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField))
	v1 := e.publishForm(t, ev.Event.ID, "")
	_, err := e.svc.GetForm(ctx, authz.Anonymous(), ev.Event.ID, consent.LocaleTR)
	// A draft event is visible to its managers only.
	wantNotFound(t, err)
	form, err := e.svc.GetForm(ctx, persona(t, "club_admin"), ev.Event.ID, consent.LocaleTR)
	if err != nil || form.Version.ID != v1.Version.ID || form.Version.VersionNo != 1 || len(form.Schema.Fields) != 1 {
		t.Fatalf("GetForm = %+v, %v", form, err)
	}
	_, err = e.svc.PublishForm(ctx, persona(t, "club_admin"), ev.Event.ID, "")
	wantCode(t, err, 409, "events.form_unchanged")
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, tshirtField))
	v2 := e.publishForm(t, ev.Event.ID, "")
	if v2.Version.VersionNo != 2 || v2.Version.ID == v1.Version.ID || bytes.Equal(v1.Version.SchemaSHA256, v2.Version.SchemaSHA256) {
		t.Fatalf("v2 = %+v", v2.Version)
	}
	f, _ := e.mem.Form(ev.Event.ID)
	if f.CurrentVersionID == nil || *f.CurrentVersionID != v2.Version.ID {
		t.Fatalf("current = %v", f.CurrentVersionID)
	}
	d, err := e.svc.GetFormDraft(ctx, persona(t, "club_admin"), ev.Event.ID)
	if err != nil || d.CurrentVersionNo == nil || *d.CurrentVersionNo != 2 {
		t.Fatalf("draft = %+v, %v", d, err)
	}
	// The event can be published now (BE-024 needs a published version).
	p, err := e.svc.PublishEvent(ctx, persona(t, "club_admin"), ev.Event.ID)
	if err != nil || p.Event.Status != events.StatusPublished {
		t.Fatalf("PublishEvent = %v", err)
	}
	form, err = e.svc.GetForm(ctx, authz.Anonymous(), ev.Event.ID, consent.LocaleTR)
	if err != nil || form.Version.VersionNo != 2 || len(form.Schema.Fields) != 2 {
		t.Fatalf("GetForm(anonymous) = %+v, %v", form, err)
	}
	if n := len(e.audit.of(authz.EventsFormPublish)); n != 2 {
		t.Fatalf("publish audit records = %d", n)
	}
}

// TestFormVersion_Immutable: a published version never changes: later
// drafts and versions leave it as it was, so a registration made on it
// still validates (ADR-0013, R-003 §2.5.2).
func TestFormVersion_Immutable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	ctx := t.Context()
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, tshirtField))
	v1 := e.publishForm(t, ev.Event.ID, "")
	before, err := e.mem.GetFormVersion(ctx, ev.Event.OrgID, v1.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	// An answer made on version 1.
	s1, err := events.DecodeFormSchema(before.Schema)
	if err != nil {
		t.Fatal(err)
	}
	old := map[string]any{"motivation": "Go öğrenmek istiyorum", "tshirt": "s"}
	if _, err := events.ValidateAnswers(s1, old); err != nil {
		t.Fatalf("v1 answers: %v", err)
	}
	// Version 2 drops option "s", relabels a field and adds an optional one.
	v2Schema := formJSON(strings.Replace(motivationField, "Neden katılmak istiyorsun?", "Motivasyonun", 1),
		`{"key":"tshirt","type":"single_select","label":{"tr":"Beden"},"required":false,
		  "options":[{"value":"s","label":{"tr":"S"},"deleted":true},{"value":"l","label":{"tr":"L"}}]}`,
		textField("school", "Okul"))
	e.putDraft(t, "club_admin", ev.Event.ID, v2Schema)
	v2 := e.publishForm(t, ev.Event.ID, "")
	after, err := e.mem.GetFormVersion(ctx, ev.Event.OrgID, v1.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Schema, after.Schema) || !bytes.Equal(before.SchemaSHA256, after.SchemaSHA256) ||
		!bytes.Equal(before.LintReport, after.LintReport) || after.VersionNo != 1 || !after.PublishedAt.Equal(before.PublishedAt) {
		t.Fatalf("version 1 changed:\n%+v\n%+v", before, after)
	}
	// The registration of version 1 is still valid on version 1, and the
	// same answer is not valid on version 2 (option s is deleted there).
	if _, err := events.ValidateAnswers(s1, old); err != nil {
		t.Fatalf("v1 answers after v2: %v", err)
	}
	s2, _ := events.DecodeFormSchema(v2.Version.Schema)
	if _, err := events.ValidateAnswers(s2, old); err == nil {
		t.Fatal("a deleted option is still accepted on version 2")
	}
	// The stored row cannot be changed through what the store returns.
	after.Schema[0] = 'X'
	again, _ := e.mem.GetFormVersion(ctx, ev.Event.OrgID, v1.Version.ID)
	if !bytes.Equal(again.Schema, before.Schema) {
		t.Fatal("the store hands out its own bytes")
	}
	// The store has no update or delete for versions: a draft event with a
	// published form cannot be deleted either (the version keeps it).
	wantCode(t, e.svc.DeleteEventDraft(ctx, persona(t, "club_admin"), ev.Event.ID), 409, "events.invalid_state")
}

// TestPublishForm_Incompatible: an earlier key keeps its type; after the
// first registration no field may become required (R-003 §2.5.2).
func TestPublishForm_Incompatible(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, tshirtField))
	e.publishForm(t, ev.Event.ID, "")
	// Reusing the key tshirt as a text field.
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, textField("tshirt", "Beden")))
	_, err := e.svc.PublishForm(t.Context(), persona(t, "club_admin"), ev.Event.ID, "")
	wantField(t, err, 422, "events.form_incompatible", "draft_schema.fields[1].type")
	// Dropping it is fine; it cannot come back with another type later.
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField))
	e.publishForm(t, ev.Event.ID, "")
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, textField("tshirt", "Beden")))
	_, err = e.svc.PublishForm(t.Context(), persona(t, "club_admin"), ev.Event.ID, "")
	wantCode(t, err, 422, "events.form_incompatible")
	// After the first registration a new field may only be optional.
	e.mem.MarkRegistered(ev.Event.ID)
	required := `{"key":"school","type":"short_text","label":{"tr":"Okul"},"required":true}`
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, required))
	_, err = e.svc.PublishForm(t.Context(), persona(t, "club_admin"), ev.Event.ID, "")
	wantField(t, err, 422, "events.form_incompatible", "draft_schema.fields[1].required")
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, textField("school", "Okul")))
	if v := e.publishForm(t, ev.Event.ID, ""); v.Version.VersionNo != 3 {
		t.Fatalf("version = %d", v.Version.VersionNo)
	}
}

// TestForm_ConsentField: a consent field needs the current consent text to
// be published; the form returns the text, and a minor gets neither the
// field nor its text (03 k12, 04 §E.1).
func TestForm_ConsentField(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.published(t, input("Sponsorlu Etkinlik"))
	e.notices.setMissing(consent.PurposeEventSponsor)
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, sponsorField))
	_, err := e.svc.PublishForm(ctx, persona(t, "club_admin"), ev.Event.ID, "")
	wantField(t, err, 422, "validation_failed", "draft_schema.fields[1].consent_purpose")

	e2 := newEnv(t)
	ev2 := e2.published(t, input("Sponsorlu Etkinlik"))
	e2.putDraft(t, "club_admin", ev2.Event.ID, formJSON(motivationField, sponsorField))
	e2.publishForm(t, ev2.Event.ID, "")
	adult, err := e2.svc.GetForm(ctx, persona(t, "student"), ev2.Event.ID, consent.LocaleTR)
	if err != nil || len(adult.Schema.Fields) != 2 || len(adult.Notices) != 1 ||
		adult.Notices[0].PurposeCode == nil || *adult.Notices[0].PurposeCode != consent.PurposeEventSponsor {
		t.Fatalf("adult form = %+v, %v", adult, err)
	}
	minor, err := e2.svc.GetForm(ctx, persona(t, "student_minor"), ev2.Event.ID, consent.LocaleTR)
	if err != nil || len(minor.Schema.Fields) != 1 || minor.Schema.Fields[0].Key != "motivation" || len(minor.Notices) != 0 {
		t.Fatalf("minor form = %+v, %v", minor, err)
	}
	// The minor's answers are checked against the same reduced schema.
	if _, err := events.ValidateAnswers(minor.Schema, map[string]any{"motivation": "x", "share_sponsor": true}); err == nil {
		t.Fatal("a minor can grant the sponsor consent")
	}
	// The DTO carries consent's NoticeDTO and the JSON Schema of the answers.
	j := string(mustJSON(t, events.ToFormDTO(adult)))
	for _, want := range []string{`"consent_notices":[{"notice_version_id"`, `"body_sha256"`, `"answers_schema":{`,
		`"$schema":"https://json-schema.org/draft/2020-12/schema"`} {
		if !strings.Contains(j, want) {
			t.Fatalf("form DTO lacks %s: %s", want, j)
		}
	}
}

// TestForm_OtherOrg_404: the draft and the publication are the organiser's
// management sub-surface: another org's admin, a non-member and the
// event's staff get 404 even on a public event; a member without the role
// 403 (06 H-38, 03 §3.6).
func TestForm_OtherOrg_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.published(t, input("Herkese Açık"))
	id := ev.Event.ID
	for _, who := range []string{"other_club_admin", "student", "event_staff", "university_rep", "company_admin"} {
		a := persona(t, who)
		_, err := e.svc.GetFormDraft(ctx, a, id)
		wantNotFound(t, err)
		_, err = e.svc.PutFormDraft(ctx, a, id, `"x"`, formJSON(motivationField))
		wantNotFound(t, err)
		_, err = e.svc.PublishForm(ctx, a, id, "")
		wantNotFound(t, err)
	}
	_, err := e.svc.GetFormDraft(ctx, persona(t, "club_member"), id)
	wantForbidden(t, err)
	// The published form itself is everyone's who sees the event.
	if _, err := e.svc.GetForm(ctx, authz.Anonymous(), id, consent.LocaleTR); err != nil {
		t.Fatalf("GetForm(anonymous) = %v", err)
	}
	// A draft event's form is invisible outside its management (404).
	draft := e.formEvent(t)
	for _, who := range []string{"club_member", "other_club_admin", "student"} {
		_, err := e.svc.GetFormDraft(ctx, persona(t, who), draft.Event.ID)
		wantNotFound(t, err)
	}
	// The store derives the form's org from the event row: a version
	// written for another org's form does not exist (06 H-38).
	f, _ := e.mem.Form(id)
	s := events.FormSchema{SchemaVersion: 1, Fields: []events.FormField{}}
	_, err = e.mem.InsertFormVersion(ctx, orgID("club_b"), events.FormVersion{ID: uuid.NewV7(), FormID: f.ID,
		Schema: events.JSON(s.Canonical()), SchemaSHA256: s.SHA256(), LintReport: events.LintReport{}.JSON(),
		PublishedBy: persona(t, "other_club_admin").UserID})
	wantNotFound(t, err)
	_, err = e.mem.UpdateFormDraft(ctx, orgID("club_b"), f.ID, events.JSON(s.Canonical()), persona(t, "other_club_admin").UserID)
	wantNotFound(t, err)
}

// TestPutDraft_StatusAndOrgRules: a cancelled event's form is final (409);
// a suspended org's admin cannot write (403); help and description are
// cleaned by the allow-list.
func TestPutDraft_StatusAndOrgRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.formEvent(t)
	help := `{"key":"bio","type":"long_text","label":{"tr":"Kısaca sen"},"required":false,
		"help":{"tr":"<p onclick=\"x()\">Bilgi <script>alert(1)</script><a href=\"javascript:alert(1)\">tıkla</a> <a href=\"https://koro.test/kvkk\">KVKK</a></p>"}}`
	raw := []byte(`{"schema_version":1,"description":{"tr":"<h2>Başvuru</h2><img src=x onerror=alert(1)>"},"fields":[` + help + `]}`)
	v := e.putDraft(t, "club_admin", ev.Event.ID, raw)
	got := v.Draft.Fields[0].Help["tr"] + v.Draft.Description["tr"]
	for _, bad := range []string{"<script", "onclick", "javascript", "onerror", "<img"} {
		if strings.Contains(strings.ToLower(got), bad) {
			t.Fatalf("help kept %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, `href="https://koro.test/kvkk"`) || !strings.Contains(got, "<h2>Başvuru</h2>") {
		t.Fatalf("allowed HTML lost: %s", got)
	}

	pub := e.published(t, input("İptal Edilecek"))
	if _, err := e.svc.CancelEvent(ctx, persona(t, "club_admin"), pub.Event.ID, "Hava durumu"); err != nil {
		t.Fatal(err)
	}
	d, _ := e.svc.GetFormDraft(ctx, persona(t, "club_admin"), pub.Event.ID)
	_, err := e.svc.PutFormDraft(ctx, persona(t, "club_admin"), pub.Event.ID, d.ETag, formJSON(motivationField))
	wantCode(t, err, 409, "events.invalid_state")
	_, err = e.svc.PublishForm(ctx, persona(t, "club_admin"), pub.Event.ID, "")
	wantCode(t, err, 409, "events.invalid_state")

	e.orgs.setStatus(orgID("club_a"), orgs.StatusSuspended)
	d, _ = e.svc.GetFormDraft(ctx, persona(t, "club_admin"), ev.Event.ID)
	_, err = e.svc.PutFormDraft(ctx, persona(t, "club_admin"), ev.Event.ID, d.ETag, formJSON(motivationField))
	wantCode(t, err, 403, "orgs.org_suspended")
}

// TestGetForm_WithoutPublishedVersion_404: 04 §2.6 "Formsuz etkinlikte 404".
func TestGetForm_WithoutPublishedVersion_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	_, err := e.svc.GetForm(t.Context(), persona(t, "club_admin"), ev.Event.ID, consent.LocaleTR)
	wantNotFound(t, err)
	_, err = e.svc.GetForm(t.Context(), authz.Anonymous(), uuid.NewV7(), consent.LocaleTR)
	wantNotFound(t, err)
}

// TestLint_FoldsTurkishText: the rules see koro_norm-ed text (upper case,
// Turkish letters, accents and joiners do not hide a field).
func TestLint_FoldsTurkishText(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	d := e.putDraft(t, "club_admin", ev.Event.ID, formJSON(
		textField("durum", "SAĞLIK DURUMUNUZ"),
		textField("kan", "Kan Grubu"),
		textField("inanc", "İnancınız"),
		`{"key":"menu","type":"single_select","label":{"tr":"Menü"},"required":false,
		  "options":[{"value":"normal","label":{"tr":"Normal"}},{"value":"vegan","label":{"tr":"Vejetaryen"}}]}`,
		textField("okul", "Okulunuz"),
	))
	var keys []string
	for _, h := range d.Lint.WarnSpecial {
		keys = append(keys, h.FieldKey)
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	if !slices.Equal(keys, []string{"durum", "inanc", "kan", "menu"}) || len(d.Lint.Blocked) != 0 {
		t.Fatalf("warn_special = %v, report %+v", keys, d.Lint)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPublishForm_IdempotencyKeyReplays: a retried publication with the same
// Idempotency-Key returns the first version instead of a 409 (06 H-5).
func TestPublishForm_IdempotencyKeyReplays(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.formEvent(t)
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(motivationField, allergyField))
	a := persona(t, "club_admin")
	ctx := idempotency.WithKey(t.Context(), idempotency.Key{Scope: a.UserID.String() + ":POST /events/{event_id}/form/versions",
		Value: "publish-form-0001", Status: 201})
	first, err := e.svc.PublishForm(ctx, a, ev.Event.ID, reason)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.PublishForm(ctx, a, ev.Event.ID, reason)
	if err != nil || again.Version.ID != first.Version.ID || again.Version.VersionNo != 1 ||
		len(again.Lint.WarnSpecial) != 1 || !bytes.Equal(again.Version.SchemaSHA256, first.Version.SchemaSHA256) {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if n := len(e.audit.of(authz.EventsFormPublish)); n != 1 {
		t.Fatalf("published %d times", n)
	}
}
