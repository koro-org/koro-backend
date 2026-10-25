package events_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/platform/problem/problemtest"
)

// TestLintRule_InvalidRE2_422: a rule pattern must compile as RE2 (Go
// regexp) and must not match the empty text (it would flag every form);
// both on creation and on update (02 §11.5, 04 §2.6).
func TestLintRule_InvalidRE2_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, dpo := t.Context(), persona(t, "dpo")
	for _, p := range []string{"(", `\btckn(`, `a{1001}`, "", "x*", "a|", string(make([]byte, 501))} {
		_, err := e.svc.CreateLintRule(ctx, dpo, events.LintRuleInput{Class: events.LintBlocked, Pattern: p,
			DescriptionTR: "Deneme kuralı"})
		wantField(t, err, 422, "validation_failed", "body.pattern")
	}
	_, err := e.svc.CreateLintRule(ctx, dpo, events.LintRuleInput{Class: "secret", Pattern: `\bx\b`, DescriptionTR: "Deneme"})
	wantField(t, err, 422, "validation_failed", "body.class")
	rules, _ := e.svc.ListLintRules(ctx, dpo)
	bad := "("
	_, err = e.svc.UpdateLintRule(ctx, dpo, rules[0].ID, events.LintRulePatch{Pattern: &bad})
	wantField(t, err, 422, "validation_failed", "body.pattern")
	if again, _ := e.svc.ListLintRules(ctx, dpo); again[0].Pattern != rules[0].Pattern {
		t.Fatal("an invalid pattern was stored")
	}
	if n := len(e.audit.Of(authz.EventsLintRuleManage)); n != 0 {
		t.Fatalf("audit records = %d", n)
	}
}

// TestLintRules_ManagedAsData: the DPO lists the 19 seeded rules, adds one,
// retires one, and the lint preview follows at once; only dpo and
// platform_admin manage rules (03 §5.6).
func TestLintRules_ManagedAsData(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, dpo := t.Context(), persona(t, "dpo")
	rules, err := e.svc.ListLintRules(ctx, dpo)
	if err != nil || len(rules) != 19 || rules[0].Class != events.LintBlocked || rules[18].Class != events.LintReviewIdentifier {
		t.Fatalf("rules = %d, %v", len(rules), err)
	}
	ev := e.formEvent(t)
	d := e.putDraft(t, "club_admin", ev.Event.ID, formJSON(textField("hobby", "Favori takımınız")))
	if len(d.Lint.Blocked)+len(d.Lint.WarnSpecial)+len(d.Lint.ReviewIdentifier) != 0 {
		t.Fatalf("lint = %+v", d.Lint)
	}
	r, err := e.svc.CreateLintRule(ctx, dpo, events.LintRuleInput{Class: events.LintWarnSpecial,
		Pattern: `\btakim`, DescriptionTR: "  Takım tutma bilgisi   gereksiz.  "})
	if err != nil || !r.Active || r.DescriptionTR != "Takım tutma bilgisi gereksiz." || r.UpdatedBy == nil || *r.UpdatedBy != dpo.UserID {
		t.Fatalf("CreateLintRule = %+v, %v", r, err)
	}
	d, _ = e.svc.GetFormDraft(ctx, persona(t, "club_admin"), ev.Event.ID)
	if len(d.Lint.WarnSpecial) != 1 || d.Lint.WarnSpecial[0].RuleID != r.ID || d.Lint.WarnSpecial[0].FieldKey != "hobby" {
		t.Fatalf("lint after the new rule = %+v", d.Lint)
	}
	off := false
	r, err = e.svc.UpdateLintRule(ctx, persona(t, "platform_admin"), r.ID, events.LintRulePatch{Active: &off})
	if err != nil || r.Active {
		t.Fatalf("UpdateLintRule = %+v, %v", r, err)
	}
	d, _ = e.svc.GetFormDraft(ctx, persona(t, "club_admin"), ev.Event.ID)
	if len(d.Lint.WarnSpecial) != 0 {
		t.Fatalf("a retired rule still matches: %+v", d.Lint)
	}
	// Reclassify a seeded rule (T-133: the lawyer may move religion to blocked).
	blocked := events.LintBlocked
	religion := rules[14]
	if religion.Class != events.LintWarnSpecial {
		t.Fatalf("seed order changed: %+v", religion)
	}
	if r, err := e.svc.UpdateLintRule(ctx, dpo, religion.ID, events.LintRulePatch{Class: &blocked}); err != nil || r.Class != blocked {
		t.Fatalf("reclassify = %+v, %v", r, err)
	}
	if n := len(e.audit.Of(authz.EventsLintRuleManage)); n != 3 {
		t.Fatalf("audit records = %d", n)
	}
	_, err = e.svc.UpdateLintRule(ctx, dpo, religion.ID, events.LintRulePatch{})
	problemtest.WantProblem(t, err, 422, "events.empty_patch")
	_, err = e.svc.UpdateLintRule(ctx, dpo, ev.Event.ID, events.LintRulePatch{Active: &off})
	wantNotFound(t, err)
	for _, who := range []string{"club_admin", "moderator", "support", "student"} {
		_, err := e.svc.ListLintRules(ctx, persona(t, who))
		wantForbidden(t, err)
		_, err = e.svc.CreateLintRule(ctx, persona(t, who), events.LintRuleInput{Class: events.LintBlocked, Pattern: `\bx\b`,
			DescriptionTR: "Deneme"})
		wantForbidden(t, err)
	}
}

// TestLint_SeedRulesClassifyLabels: the seeded rules (DefaultLintRules =
// migration 000060) on the field titles organisers really write. The
// national ID number is the canonical blocked field (02 §11.4), so its
// suffixed and abbreviated forms ("Kimlik numaranız", "TC No") must be
// caught; ordinary registration fields must stay clean.
func TestLint_SeedRulesClassifyLabels(t *testing.T) {
	t.Parallel()
	const (
		blocked = "blocked"
		warn    = "warn_special"
		review  = "review_identifier"
		clean   = ""
	)
	for label, want := range map[string]string{
		"T.C. Kimlik Numaranız":               blocked,
		"TC Kimlik No":                        blocked,
		"TCKN":                                blocked,
		"Kimlik No":                           blocked,
		"Kimlik Numarası":                     blocked,
		"Kimlik numaranız":                    blocked,
		"Türkiye Cumhuriyeti kimlik numarası": blocked,
		"TC No":                               blocked,
		"T.C. No":                             blocked,
		"Nüfus cüzdanı seri no":               blocked,
		"Vatandaşlık numarası":                blocked,
		"Kredi kartı numarası":                blocked,
		"IBAN":                                blocked,
		"Şifreniz":                            blocked,
		"Siyasi görüşünüz":                    blocked,
		"Sağlık durumunuz":                    warn,
		"Hamile misiniz?":                     warn,
		"Psikolojik destek alıyor musunuz?":   warn,
		"Engel durumunuz":                     warn,
		"Herhangi bir engeliniz var mı?":      warn,
		"Kan grubu":                           warn,
		"Dininiz":                             warn,
		"Pasaport numarası":                   review,
		"Ehliyet numaranız":                   review,
		"Ad Soyad":                            clean,
		"Telefon numaranız":                   clean,
		"Öğrenci numaranız":                   clean,
		"Kimlik kartınızı yanınızda getirin":  clean,
		"Psikoloji bölümü öğrencisi misiniz?": clean,
		"Bölümünüz":        clean,
		"Tişört bedeniniz": clean,
	} {
		s := events.FormSchema{SchemaVersion: events.FormSchemaVersion, Fields: []events.FormField{
			{Key: "alan", Type: events.FieldShortText, Label: events.LocalText{"tr": label}},
		}}
		r := events.LintSchema(s, events.DefaultLintRules())
		got := clean
		switch {
		case len(r.Blocked) > 0:
			got = blocked
		case len(r.WarnSpecial) > 0:
			got = warn
		case len(r.ReviewIdentifier) > 0:
			got = review
		}
		if got != want {
			t.Errorf("lint of %q = %q, want %q (report %+v)", label, got, want, r)
		}
	}
}
