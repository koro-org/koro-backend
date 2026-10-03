package consent_test

import (
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/problem"
)

func privacyDraft(aud consent.Audience, locale, version string) consent.NoticeDraft {
	return consent.NoticeDraft{Kind: consent.KindPrivacyNotice, Audience: aud, Locale: locale, Version: version,
		Title: "Aydınlatma Metni " + version, BodyMD: "# Aydınlatma\r\n\r\nSürüm " + version + ".\r\n"}
}

// TestPublishNotice_RetiresPreviousSameKey (04 §2.5): publishing a version
// retires the current version of the same (kind, purpose, audience,
// locale) in the same step; other keys are untouched.
func TestPublishNotice_RetiresPreviousSameKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	dpo := actor(t, "dpo")
	v1 := e.publish(t, privacyDraft(consent.AudienceStudent, consent.LocaleTR, "2026.10.01"))
	otherLocale := e.publish(t, privacyDraft(consent.AudienceStudent, consent.LocaleEN, "2026.10.01"))
	otherAudience := e.publish(t, privacyDraft(consent.AudienceCompany, consent.LocaleTR, "2026.10.01"))
	email := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	push := e.consentText(t, consent.PurposeCommercialPush, "2026.10.01")

	e.clk.Advance(time.Hour)
	v2, err := e.svc.CreateNotice(ctx, dpo, privacyDraft(consent.AudienceStudent, consent.LocaleTR, "2026.10.02"), proof)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Status() != consent.StatusDraft || !strings.Contains(v2.BodyMD, "\n\nSürüm") || strings.Contains(v2.BodyMD, "\r") {
		t.Fatalf("draft = %+v", v2)
	}
	cur, err := e.svc.Current(ctx, consent.NoticeKey{Kind: consent.KindPrivacyNotice, Audience: consent.AudienceStudent, Locale: consent.LocaleTR})
	if err != nil || cur.ID != v1.ID {
		t.Fatalf("Current before publishing = %v, %v; want v1", cur.ID, err)
	}

	v2, err = e.svc.PublishNotice(ctx, dpo, v2.ID, proof)
	if err != nil || v2.Status() != consent.StatusPublished || !v2.PublishedAt.Equal(e.clk.Now()) {
		t.Fatalf("PublishNotice = %+v, %v", v2, err)
	}
	old, _ := e.store.Notice(ctx, v1.ID)
	if old.Status() != consent.StatusRetired || !old.RetiredAt.Equal(*v2.PublishedAt) {
		t.Fatalf("v1 after publishing v2 = %s retired %v", old.Status(), old.RetiredAt)
	}
	for _, n := range []consent.Notice{otherLocale, otherAudience, email, push} {
		if got, _ := e.store.Notice(ctx, n.ID); got.Status() != consent.StatusPublished {
			t.Fatalf("notice %s %s/%s changed to %s", n.Kind, n.Audience, n.Locale, got.Status())
		}
	}
	cur, _ = e.svc.Current(ctx, consent.NoticeKey{Kind: consent.KindPrivacyNotice, Audience: consent.AudienceStudent, Locale: consent.LocaleTR})
	if cur.ID != v2.ID {
		t.Fatalf("Current after publishing = %v, want v2", cur.ID)
	}

	// A consent text: the new version takes over its own purpose only.
	purpose := consent.PurposeCommercialEmail
	email2 := e.consentText(t, purpose, "2026.10.02")
	if got, _ := e.store.Notice(ctx, email.ID); got.Status() != consent.StatusRetired {
		t.Fatalf("old consent text %s", got.Status())
	}
	if got, _ := e.store.Notice(ctx, push.ID); got.Status() != consent.StatusPublished {
		t.Fatalf("consent text of another purpose %s", got.Status())
	}
	if cur, _ := e.svc.Current(ctx, consent.NoticeKey{Kind: consent.KindConsentText, Purpose: &purpose,
		Audience: consent.AudienceAll, Locale: consent.LocaleTR}); cur.ID != email2.ID {
		t.Fatalf("current consent text %v, want %v", cur.ID, email2.ID)
	}

	// The audit row names the retired version and the reauth kind.
	var publishes []audit.Event
	for _, ev := range e.audit.of(string(authz.ConsentNoticeManage)) {
		if ev.Metadata["step"] == "publish" && *ev.ResourceID == v2.ID {
			publishes = append(publishes, ev)
		}
	}
	if len(publishes) != 1 || publishes[0].Metadata["retired_notice_id"] != v1.ID.String() || publishes[0].Metadata[audit.MetaReauth] != "password" {
		t.Fatalf("publish audit = %+v", publishes)
	}

	// State machine: no second publication, no retirement of a draft or of a retired version.
	_, err = e.svc.PublishNotice(ctx, dpo, v2.ID, proof)
	wantErr(t, err, consent.ErrNoticeState)
	_, err = e.svc.RetireNotice(ctx, dpo, v1.ID, proof)
	wantErr(t, err, consent.ErrNoticeState)
	d, _ := e.svc.CreateNotice(ctx, dpo, privacyDraft(consent.AudienceAll, consent.LocaleTR, "2026.10.05"), proof)
	_, err = e.svc.RetireNotice(ctx, dpo, d.ID, proof)
	wantErr(t, err, consent.ErrNoticeState)
	// The same version twice for one key.
	_, err = e.svc.CreateNotice(ctx, dpo, privacyDraft(consent.AudienceStudent, consent.LocaleTR, "2026.10.02"), proof)
	wantErr(t, err, consent.ErrNoticeVersionTaken)
}

// TestNotice_RetiredVersionStillReadable (04 §2.5): a retired version is
// still served by getNoticeVersion (it proves earlier consents) but is no
// longer current and accepts no new grant.
func TestNotice_RetiredVersionStillReadable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	user := actor(t, "student").UserID
	text := e.consentText(t, consent.PurposeContactInfoShare, "2026.10.01")
	e.record(t, grant(user, text, consent.ChannelWeb))
	if _, err := e.svc.RetireNotice(ctx, actor(t, "dpo"), text.ID, proof); err != nil {
		t.Fatal(err)
	}

	for _, a := range []authz.Actor{authz.Anonymous(), actor(t, "student"), actor(t, "company_member")} {
		n, err := e.svc.GetNotice(ctx, a, text.ID)
		if err != nil || n.Status() != consent.StatusRetired || n.BodyMD != text.BodyMD {
			t.Fatalf("GetNotice(retired) as %s = %+v, %v", a, n, err)
		}
	}
	list, err := e.svc.ListCurrentNotices(ctx, authz.Anonymous(), consent.NoticeFilter{Audience: consent.AudienceAll, Locale: consent.LocaleTR})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range list {
		if n.ID == text.ID {
			t.Fatal("a retired version is listed as current")
		}
	}
	_, err = e.svc.Record(ctx, grant(actor(t, "student_b").UserID, text, consent.ChannelWeb))
	wantErr(t, err, consent.ErrNoticeNotCurrent)
	// Withdrawing with the retired text as reference stays possible.
	in := grant(user, text, consent.ChannelWeb)
	in.Action = consent.Withdrawn
	if _, err := e.svc.Record(ctx, in); err != nil {
		t.Fatalf("withdraw citing a retired text: %v", err)
	}
	// A draft is invisible to everyone but dpo.
	d, _ := e.svc.CreateNotice(ctx, actor(t, "dpo"), privacyDraft(consent.AudienceAll, consent.LocaleTR, "2026.12.01"), proof)
	_, err = e.svc.GetNotice(ctx, authz.Anonymous(), d.ID)
	wantErr(t, err, authz.ErrNotFound)
	if _, err := e.svc.GetNotice(ctx, actor(t, "dpo"), d.ID); err != nil {
		t.Fatalf("dpo GetNotice(draft) = %v", err)
	}
	_, err = e.svc.GetNotice(ctx, authz.Anonymous(), uuid.NewV7())
	wantErr(t, err, consent.ErrNotFound)
}

// TestCurrentNotices_Fallbacks (04 §1.12): the reader's audience before
// "all", the reader's locale before tr-TR, one version per kind and purpose.
func TestCurrentNotices_Fallbacks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	allTR := e.publish(t, privacyDraft(consent.AudienceAll, consent.LocaleTR, "2026.10.01"))
	studentTR := e.publish(t, privacyDraft(consent.AudienceStudent, consent.LocaleTR, "2026.10.01"))
	allEN := e.publish(t, privacyDraft(consent.AudienceAll, consent.LocaleEN, "2026.10.01"))
	terms := e.publish(t, consent.NoticeDraft{Kind: consent.KindTermsOfUse, Audience: consent.AudienceAll, Locale: consent.LocaleTR,
		Version: "2026.10.01", Title: "Koşullar", BodyMD: "Koşullar.\n"})
	email := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")

	pick := func(aud consent.Audience, locale string) map[consent.NoticeKind]uuid.UUID {
		t.Helper()
		rows, err := e.svc.ListCurrentNotices(ctx, authz.Anonymous(), consent.NoticeFilter{Audience: aud, Locale: locale})
		if err != nil {
			t.Fatal(err)
		}
		out := map[consent.NoticeKind]uuid.UUID{}
		for _, n := range rows {
			if _, dup := out[n.Kind]; dup {
				t.Fatalf("two current %s notices for %s/%s", n.Kind, aud, locale)
			}
			out[n.Kind] = n.ID
		}
		return out
	}
	if got := pick(consent.AudienceStudent, consent.LocaleTR); got[consent.KindPrivacyNotice] != studentTR.ID ||
		got[consent.KindTermsOfUse] != terms.ID || got[consent.KindConsentText] != email.ID {
		t.Fatalf("student/tr = %v", got)
	}
	if got := pick(consent.AudienceCompany, consent.LocaleTR); got[consent.KindPrivacyNotice] != allTR.ID {
		t.Fatalf("company/tr falls back to all: %v", got)
	}
	if got := pick(consent.AudienceStudent, consent.LocaleEN); got[consent.KindPrivacyNotice] != allEN.ID || got[consent.KindTermsOfUse] != terms.ID {
		t.Fatalf("student/en = %v (audience all in English beats the student text in Turkish)", got)
	}

	purpose := consent.PurposeCommercialEmail
	n, err := e.svc.Current(ctx, consent.NoticeKey{Kind: consent.KindConsentText, Purpose: &purpose, Audience: consent.AudienceStudent, Locale: consent.LocaleEN})
	if err != nil || n.ID != email.ID {
		t.Fatalf("Current(consent text, student, en) = %v, %v", n.ID, err)
	}
	_, err = e.svc.Current(ctx, consent.NoticeKey{Kind: consent.KindCookieNotice, Audience: consent.AudienceAll, Locale: consent.LocaleTR})
	wantErr(t, err, consent.ErrNoNotice)
}

// TestCreateNotice_Validation (02 §4.2, Kurul 2026/347).
func TestCreateNotice_Validation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	dpo := actor(t, "dpo")
	email := consent.PurposeCommercialEmail
	base := privacyDraft(consent.AudienceAll, consent.LocaleTR, "2026.10.01")
	tests := []struct {
		name string
		mod  func(*consent.NoticeDraft)
		want error
	}{
		{"consent text without purpose", func(d *consent.NoticeDraft) { d.Kind = consent.KindConsentText }, consent.ErrInvalidNotice},
		{"privacy notice with a purpose", func(d *consent.NoticeDraft) { d.Purpose = &email }, consent.ErrInvalidNotice},
		{"terms with a purpose", func(d *consent.NoticeDraft) { d.Kind, d.Purpose = consent.KindTermsOfUse, &email }, consent.ErrInvalidNotice},
		{"unknown purpose", func(d *consent.NoticeDraft) {
			d.Kind, d.Purpose = consent.KindConsentText, ptr(consent.Purpose("selfie_share"))
		}, consent.ErrUnknownPurpose},
		{"bad version", func(d *consent.NoticeDraft) { d.Version = "v2" }, consent.ErrInvalidNotice},
		{"bad locale", func(d *consent.NoticeDraft) { d.Locale = "de-DE" }, consent.ErrInvalidNotice},
		{"blank title", func(d *consent.NoticeDraft) { d.Title = " \t " }, consent.ErrInvalidNotice},
		{"long title", func(d *consent.NoticeDraft) { d.Title = strings.Repeat("ş", 201) }, consent.ErrInvalidNotice},
		{"blank body", func(d *consent.NoticeDraft) { d.BodyMD = "\n\n" }, consent.ErrInvalidNotice},
		{"bidi override in body", func(d *consent.NoticeDraft) { d.BodyMD = "Kabul \u202eetmiyorum" }, consent.ErrInvalidNotice},
		{"control character in body", func(d *consent.NoticeDraft) { d.BodyMD = "a\x00b" }, consent.ErrInvalidNotice},
	}
	for _, tt := range tests {
		d := base
		tt.mod(&d)
		_, err := e.svc.CreateNotice(ctx, dpo, d, proof)
		if err == nil {
			t.Fatalf("%s: created", tt.name)
		}
		wantErr(t, err, tt.want)
	}

	// The digest is the SHA-256 of the stored body (LF line breaks).
	n, err := e.svc.CreateNotice(ctx, dpo, base, proof)
	if err != nil {
		t.Fatal(err)
	}
	if string(n.BodySHA256) != string(consent.BodyDigest(n.BodyMD)) || n.CreatedBy == nil || *n.CreatedBy != dpo.UserID {
		t.Fatalf("draft = %+v", n)
	}
}

// TestNoticeManage_ReauthAndAuthz (03 §6.2): authz first, then the proof;
// a wrong proof is audited as denied and changes nothing.
func TestNoticeManage_ReauthAndAuthz(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	d := privacyDraft(consent.AudienceAll, consent.LocaleTR, "2026.10.01")

	_, err := e.svc.CreateNotice(ctx, actor(t, "platform_admin"), d, proof)
	wantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.CreateNotice(ctx, actor(t, "dpo"), d, consent.ReauthProof{Password: "wrong"})
	wantErr(t, err, errBadProof)
	denied := e.audit.of(string(authz.ConsentNoticeManage))
	if len(denied) != 1 || denied[0].Outcome != audit.Denied || denied[0].Reason != "reauth_failed" {
		t.Fatalf("audit of the failed proof = %+v", denied)
	}
	if rows, _ := e.store.CurrentNotices(ctx, consent.CurrentQuery{}); len(rows) != 0 {
		t.Fatalf("notices after refused calls: %d", len(rows))
	}
	// The proof is never printed.
	if s := (consent.ReauthProof{Password: "hunter2"}).String(); strings.Contains(s, "hunter2") {
		t.Fatalf("ReauthProof.String() = %q", s)
	}

	// Without a verifier (identity not wired) R operations fail closed.
	closed := newEnv(t, func(d *consent.Deps) { d.Reauth = nil })
	_, err = closed.svc.CreateNotice(ctx, actor(t, "dpo"), d, proof)
	wantErr(t, err, consent.ErrReauthUnavailable)
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Status() != 503 {
		t.Fatalf("unwired reauth = %v", err)
	}
}

// TestUpdatePurpose (consent.purpose.manage, R).
func TestUpdatePurpose(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	dpo := actor(t, "dpo")

	_, err := e.svc.UpdatePurpose(ctx, dpo, consent.PurposeCommercialPush, consent.PurposePatch{}, proof)
	wantErr(t, err, consent.ErrEmptyPatch)
	_, err = e.svc.UpdatePurpose(ctx, dpo, "unknown_purpose", consent.PurposePatch{Active: ptr(true)}, proof)
	wantErr(t, err, consent.ErrNotFound)
	_, err = e.svc.UpdatePurpose(ctx, actor(t, "support"), consent.PurposeCommercialPush, consent.PurposePatch{Active: ptr(true)}, proof)
	wantErr(t, err, authz.ErrForbidden)

	p, err := e.svc.UpdatePurpose(ctx, dpo, consent.PurposeCommercialPush,
		consent.PurposePatch{Active: ptr(false), DescriptionTR: ptr("  Mobil   kampanya bildirimleri. ")}, proof)
	if err != nil || p.Active || p.DescriptionTR != "Mobil kampanya bildirimleri." {
		t.Fatalf("UpdatePurpose = %+v, %v", p, err)
	}
	list, err := e.svc.ListPurposes(ctx, dpo)
	if err != nil || len(list) != 7 {
		t.Fatalf("ListPurposes = %d, %v", len(list), err)
	}
	evs := e.audit.of(string(authz.ConsentPurposeManage))
	if len(evs) != 1 || evs[0].Metadata["active"] != false || evs[0].Metadata["description_changed"] != true {
		t.Fatalf("audit = %+v", evs)
	}
}
