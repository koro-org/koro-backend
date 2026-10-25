package consent_test

import (
	"errors"
	"net/http"
	"testing"

	"koro/internal/consent"
	"koro/internal/platform/problem"
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
		wantErr(t, err, consent.ErrIYSUnavailable)
		pe, ok := errors.AsType[*problem.Error](err)
		if !ok || pe.Status() != http.StatusConflict || pe.Code() != "consent.iys_unavailable" {
			t.Fatalf("%s grant without İYS = %v, want 409 consent.iys_unavailable", purpose, err)
		}
		id := text.ID
		_, err = off.svc.RecordMine(ctx, student, consent.MyConsent{Purpose: purpose, Action: consent.Granted,
			NoticeVersionID: &id, TextSHA256: text.BodySHA256, Channel: consent.ChannelMobile})
		wantErr(t, err, consent.ErrIYSUnavailable)
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
