package consent_test

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
	"time"
	"uuid"

	"koro/internal/consent"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem/problemtest"
)

// TestRecordTerms_RecordsCurrentVersion (BE-118, Q-17): an acceptance names
// the current terms of use of the reader's audience (or of audience all),
// carries the notice's body digest, the channel and the request ID, and is
// audited as the subject's own act. It never touches the consent ledger.
func TestRecordTerms_RecordsCurrentVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := obs.WithRequestID(t.Context(), "req-BE118-terms-1")
	n := e.terms(t, consent.AudienceAll, "2026.10.01")
	u := uuid.NewV7()
	got, err := e.svc.RecordTerms(ctx, consent.TermsInput{UserID: u, NoticeVersionID: n.ID, Audience: consent.AudienceStudent,
		Channel: consent.ChannelMobile})
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != u || got.NoticeVersionID != n.ID || !bytes.Equal(got.TextSHA256, n.BodySHA256) ||
		got.Channel != consent.ChannelMobile || got.RequestID != "req-BE118-terms-1" || !got.AcceptedAt.Equal(e.clk.Now()) {
		t.Fatalf("acceptance = %+v", got)
	}
	rows, err := e.store.TermsAcceptancesOf(ctx, u)
	if err != nil || len(rows) != 1 || rows[0].ID != got.ID || rows[0].CreatedAt.IsZero() {
		t.Fatalf("ledger = %+v, %v", rows, err)
	}
	evs := e.audit.of("terms.accepted")
	if len(evs) != 1 || evs[0].ActorUserID == nil || *evs[0].ActorUserID != u || *evs[0].ResourceID != got.ID ||
		evs[0].Metadata["notice_version_id"] != n.ID.String() || evs[0].Metadata["version"] != "2026.10.01" ||
		evs[0].Metadata["channel"] != "mobile" {
		t.Fatalf("audit = %+v", evs)
	}
	if c, _ := e.store.AllOf(ctx, u); len(c) != 0 || len(e.out.Events()) != 0 {
		t.Fatalf("an acceptance wrote consents %v or events %v", c, e.out.Events())
	}

	// The same version again: no second row, no second audit, same answer.
	again, err := e.svc.RecordTerms(ctx, consent.TermsInput{UserID: u, NoticeVersionID: n.ID, Audience: consent.AudienceStudent,
		Channel: consent.ChannelWeb})
	if err != nil || again.ID != got.ID {
		t.Fatalf("second acceptance = %+v, %v", again, err)
	}
	if rows, _ := e.store.TermsAcceptancesOf(ctx, u); len(rows) != 1 || len(e.audit.of("terms.accepted")) != 1 {
		t.Fatalf("second acceptance wrote: %d rows, %d audits", len(rows), len(e.audit.of("terms.accepted")))
	}
}

// TestRecordTerms_RefusesAnythingButTheCurrentTerms: a retired version, a
// draft, an unknown ID, another kind and another audience are all
// consent.terms_not_current (422); nothing is written.
func TestRecordTerms_RefusesAnythingButTheCurrentTerms(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	old := e.terms(t, consent.AudienceAll, "2026.09.01")
	cur := e.terms(t, consent.AudienceAll, "2026.10.01") // retires old
	company := e.terms(t, consent.AudienceCompany, "2026.10.01")
	privacyNotice := e.publish(t, consent.NoticeDraft{Kind: consent.KindPrivacyNotice, Audience: consent.AudienceAll,
		Locale: consent.LocaleTR, Version: "2026.10.01", Title: "Aydınlatma", BodyMD: "Aydınlatma metni\n"})
	draft, err := e.svc.CreateNotice(ctx, actor(t, "dpo"), consent.NoticeDraft{Kind: consent.KindTermsOfUse, Audience: consent.AudienceAll,
		Locale: consent.LocaleTR, Version: "2026.11.01", Title: "Taslak", BodyMD: "Taslak koşullar\n"}, proof)
	if err != nil {
		t.Fatal(err)
	}
	u := uuid.NewV7()
	for name, id := range map[string]uuid.UUID{
		"retired version": old.ID, "draft": draft.ID, "unknown": uuid.NewV7(), "nil": uuid.Nil(),
		"privacy notice": privacyNotice.ID, "another audience": company.ID,
	} {
		_, err := e.svc.RecordTerms(ctx, consent.TermsInput{UserID: u, NoticeVersionID: id, Audience: consent.AudienceStudent,
			Channel: consent.ChannelWeb})
		if !errors.Is(err, consent.ErrTermsNotCurrent) {
			t.Errorf("%s = %v, want consent.terms_not_current", name, err)
		}
	}
	_, err = e.svc.RecordTerms(ctx, consent.TermsInput{UserID: u, NoticeVersionID: old.ID, Audience: consent.AudienceAll,
		Channel: consent.ChannelWeb})
	if st, code := problemtest.StatusCode(err); st != http.StatusUnprocessableEntity || code != "consent.terms_not_current" {
		t.Fatalf("retired version = %d %s, want 422 consent.terms_not_current", st, code)
	}
	if rows, _ := e.store.TermsAcceptancesOf(ctx, u); len(rows) != 0 || len(e.audit.of("terms.accepted")) != 0 {
		t.Fatalf("refused acceptances wrote %d rows", len(rows))
	}
	// A student reader cannot accept the generic version while a student
	// version is current (Notices.Current resolves the audience first); a
	// university reader, who has no version of their own, can.
	student := e.terms(t, consent.AudienceStudent, "2026.10.02")
	if err := e.svc.CheckTerms(ctx, cur.ID, consent.AudienceStudent); !errors.Is(err, consent.ErrTermsNotCurrent) {
		t.Fatalf("generic terms for a student while a student version is current = %v", err)
	}
	if err := e.svc.CheckTerms(ctx, student.ID, consent.AudienceStudent); err != nil {
		t.Fatalf("student terms for a student = %v", err)
	}
	if err := e.svc.CheckTerms(ctx, cur.ID, consent.AudienceUniversity); err != nil {
		t.Fatalf("generic terms for a university reader = %v", err)
	}
	// The company's own terms are current for a company reader.
	if _, err := e.svc.RecordTerms(ctx, consent.TermsInput{UserID: u, NoticeVersionID: company.ID, Audience: consent.AudienceCompany,
		Channel: consent.ChannelWeb}); err != nil {
		t.Fatalf("company terms for a company reader = %v", err)
	}
	// Channels that show no terms are a programming error, never a row.
	for _, ch := range []consent.Channel{consent.ChannelEmailLink, consent.ChannelAdmin, consent.ChannelIYS} {
		if _, err := e.svc.RecordTerms(ctx, consent.TermsInput{UserID: u, NoticeVersionID: cur.ID, Audience: consent.AudienceAll,
			Channel: ch}); err == nil {
			t.Fatalf("acceptance through %s", ch)
		}
	}
}

// TestTermsStatus_NewVersionRequiresAcceptance: no terms published → nothing
// to accept; a new version makes the earlier acceptance insufficient
// (Required, Accepted = the old one) until the new one is accepted; an
// audience falls back to "all".
func TestTermsStatus_NewVersionRequiresAcceptance(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := uuid.NewV7()
	st, err := e.svc.TermsStatus(ctx, u, consent.AudienceStudent, consent.LocaleEN)
	if err != nil || st.Current != nil || st.Accepted != nil || st.Required {
		t.Fatalf("no terms published: %+v, %v", st, err)
	}
	v1 := e.terms(t, consent.AudienceAll, "2026.10.01")
	if st, _ := e.svc.TermsStatus(ctx, u, consent.AudienceStudent, consent.LocaleEN); st.Current == nil || st.Current.ID != v1.ID ||
		!st.Required || st.Accepted != nil {
		t.Fatalf("never accepted: %+v", st)
	}
	accept := func(n consent.Notice) {
		t.Helper()
		if _, err := e.svc.RecordTerms(ctx, consent.TermsInput{UserID: u, NoticeVersionID: n.ID, Audience: consent.AudienceStudent,
			Channel: consent.ChannelWeb}); err != nil {
			t.Fatal(err)
		}
	}
	accept(v1)
	if st, _ := e.svc.TermsStatus(ctx, u, consent.AudienceStudent, consent.LocaleTR); st.Required || st.Accepted == nil ||
		st.Accepted.NoticeVersionID != v1.ID {
		t.Fatalf("accepted v1: %+v", st)
	}
	e.clk.Advance(24 * time.Hour)
	v2 := e.terms(t, consent.AudienceAll, "2026.11.01")
	st, _ = e.svc.TermsStatus(ctx, u, consent.AudienceStudent, consent.LocaleTR)
	if !st.Required || st.Current.ID != v2.ID || st.Accepted == nil || st.Accepted.NoticeVersionID != v1.ID {
		t.Fatalf("v2 published: %+v", st)
	}
	accept(v2)
	st, _ = e.svc.TermsStatus(ctx, u, consent.AudienceStudent, consent.LocaleTR)
	if st.Required || st.Accepted.NoticeVersionID != v2.ID {
		t.Fatalf("accepted v2: %+v", st)
	}
	// A student-specific version wins over "all" for students only.
	sv := e.terms(t, consent.AudienceStudent, "2026.11.02")
	if st, _ := e.svc.TermsStatus(ctx, u, consent.AudienceStudent, consent.LocaleTR); !st.Required || st.Current.ID != sv.ID {
		t.Fatalf("student version: %+v", st)
	}
	if st, _ := e.svc.TermsStatus(ctx, u, consent.AudienceCompany, consent.LocaleTR); st.Required || st.Current.ID != v2.ID {
		t.Fatalf("company reader: %+v", st)
	}
	if _, err := e.svc.TermsStatus(ctx, u, "alien", consent.LocaleTR); err == nil {
		t.Fatal("unknown audience accepted")
	}
}

// TestTermsAcceptance_ExportedAndKeptOnErasure: the KVKK export lists the
// acceptances; the erasure keeps them as contract proof (terms_acceptance
// starts at the erasure) and reports them retained.
func TestTermsAcceptance_ExportedAndKeptOnErasure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := obs.WithRequestID(t.Context(), "req-BE118-export")
	n := e.terms(t, consent.AudienceAll, "2026.10.01")
	u := uuid.NewV7()
	a, err := e.svc.RecordTerms(ctx, consent.TermsInput{UserID: u, NoticeVersionID: n.ID, Audience: consent.AudienceAll, Channel: consent.ChannelWeb})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := e.mod.Privacy.Export(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, it := range sec.Items {
		if it["terms_acceptance_id"] == a.ID.String() {
			found = it
		}
	}
	if found == nil || found["notice_version_id"] != n.ID.String() || found["channel"] != "web" ||
		found["request_id"] != "req-BE118-export" || found["text_sha256"] == "" || found["accepted_at"] == "" {
		t.Fatalf("export items = %v", sec.Items)
	}
	res, err := e.mod.Privacy.Erase(ctx, u)
	if err != nil || res.Retained != 1 || res.RetainedReason == "" {
		t.Fatalf("erase = %+v, %v", res, err)
	}
	if rows, _ := e.store.TermsAcceptancesOf(ctx, u); len(rows) != 1 {
		t.Fatalf("erasure deleted the acceptance: %d rows", len(rows))
	}
}
