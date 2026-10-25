package consent_test

import (
	"net/http"
	"testing"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/problem/problemtest"
)

// TestRecord_CommercialGrantNeedsIYS (BE-105, T-364): while no İYS adapter
// is configured (CONSENT_IYS_PROVIDER=none) a grant of a commercial
// purpose (requires_iys) is refused with 409 consent.iys_unavailable and
// leaves nothing behind: a consent taken outside İYS is valid only once
// İYS registers it within three business days (Yönetmelik m.7/11–12).
// Withdrawals and purposes without İYS are not affected; with an adapter
// the grant is taken.
func TestRecord_CommercialGrantNeedsIYS(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	student := actor(t, "student")

	off := newEnv(t, func(d *consent.Deps) { d.IYS = nil })
	for _, purpose := range []consent.Purpose{consent.PurposeCommercialEmail, consent.PurposeCommercialSMS} {
		text := off.consentText(t, purpose, "2026.10.01")
		_, err := off.svc.Record(ctx, grant(student.UserID, text, consent.ChannelWeb))
		problemtest.WantErr(t, err, consent.ErrIYSUnavailable)
		problemtest.WantProblem(t, err, http.StatusConflict, "consent.iys_unavailable")
		id := text.ID
		_, err = off.svc.RecordMine(ctx, student, consent.MyConsent{Purpose: purpose, Action: consent.Granted,
			NoticeVersionID: &id, TextSHA256: text.BodySHA256, Channel: consent.ChannelMobile})
		problemtest.WantErr(t, err, consent.ErrIYSUnavailable)
	}
	if rows, err := off.store.AllOf(ctx, student.UserID); err != nil || len(rows) != 0 {
		t.Fatalf("refused grants left %d consent rows (%v)", len(rows), err)
	}
	if n := len(off.audit.of("consent.record.create")); n != 0 {
		t.Fatalf("refused grants were audited %d times", n)
	}
	if n := off.recordedEvents(); n != 0 {
		t.Fatalf("refused grants wrote %d consent.recorded events", n)
	}
	// A withdrawal is always taken (03 k12); a purpose without İYS is
	// granted as before.
	off.record(t, withdraw(student.UserID, consent.PurposeCommercialEmail))
	off.record(t, grant(student.UserID, off.consentText(t, consent.PurposeCommercialPush, "2026.10.01"), consent.ChannelWeb))

	on := newEnv(t) // FakeIYS: an adapter is configured
	on.record(t, grant(student.UserID, on.consentText(t, consent.PurposeCommercialEmail, "2026.10.01"), consent.ChannelWeb))
}

// TestCurrentNotices_Collectable (BE-114, T-367): listCurrentNotices says
// for every consent text whether its purpose is collectable now, from the
// same rule that refuses the grant: without an İYS adapter a commercial
// purpose (requires_iys) is not, a purpose without İYS is; a switched-off
// purpose is not; with an adapter the commercial purpose is. Other kinds
// carry no flag.
func TestCurrentNotices_Collectable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	list := func(t *testing.T, e *env) map[consent.Purpose]*bool {
		t.Helper()
		rows, err := e.svc.ListCurrentNotices(ctx, authz.Anonymous(), consent.NoticeFilter{Audience: consent.AudienceStudent, Locale: consent.LocaleTR})
		if err != nil {
			t.Fatal(err)
		}
		out := map[consent.Purpose]*bool{}
		for _, n := range rows {
			switch {
			case n.Kind != consent.KindConsentText && n.Collectable != nil:
				t.Fatalf("%s notice carries collectable", n.Kind)
			case n.Kind == consent.KindConsentText && n.Collectable == nil:
				t.Fatalf("consent text of %s without collectable", *n.PurposeCode)
			case n.Kind == consent.KindConsentText:
				out[*n.PurposeCode] = n.Collectable
			}
		}
		return out
	}
	setup := func(e *env) {
		e.publish(t, privacyDraft(consent.AudienceAll, consent.LocaleTR, "2026.10.01"))
		for _, p := range []consent.Purpose{consent.PurposeCommercialEmail, consent.PurposeCommercialSMS, consent.PurposeCommercialPush,
			consent.PurposeContactInfoShare} {
			e.consentText(t, p, "2026.10.01")
		}
	}

	off := newEnv(t, func(d *consent.Deps) { d.IYS = nil }) // CONSENT_IYS_PROVIDER=none
	setup(off)
	got := list(t, off)
	for p, want := range map[consent.Purpose]bool{consent.PurposeCommercialEmail: false, consent.PurposeCommercialSMS: false,
		consent.PurposeCommercialPush: true, consent.PurposeContactInfoShare: true} {
		if got[p] == nil || *got[p] != want {
			t.Errorf("without İYS: %s collectable = %v, want %v", p, got[p], want)
		}
	}
	// The flag and the gate agree: what is not collectable is refused.
	student := actor(t, "student")
	text, err := off.svc.Current(ctx, consent.NoticeKey{Kind: consent.KindConsentText, Purpose: ptr(consent.PurposeCommercialEmail),
		Audience: consent.AudienceStudent, Locale: consent.LocaleTR})
	if err != nil {
		t.Fatal(err)
	}
	_, err = off.svc.Record(ctx, grant(student.UserID, text, consent.ChannelWeb))
	problemtest.WantErr(t, err, consent.ErrIYSUnavailable)
	// A switched-off purpose is not collectable either.
	if _, err := off.svc.UpdatePurpose(ctx, actor(t, "dpo"), consent.PurposeContactInfoShare,
		consent.PurposePatch{Active: ptr(false)}, proof); err != nil {
		t.Fatal(err)
	}
	if got := list(t, off); *got[consent.PurposeContactInfoShare] {
		t.Error("a switched-off purpose is collectable")
	}

	on := newEnv(t) // an İYS adapter is configured (FakeIYS)
	setup(on)
	if got := list(t, on); !*got[consent.PurposeCommercialEmail] || !*got[consent.PurposeCommercialSMS] {
		t.Errorf("with İYS: commercial_email %v, commercial_sms %v; want collectable", *got[consent.PurposeCommercialEmail],
			*got[consent.PurposeCommercialSMS])
	}
}
