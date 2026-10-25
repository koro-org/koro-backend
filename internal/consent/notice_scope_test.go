package consent_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/consent"
)

// TestCurrentNotices_ScopeType (BE-123): listCurrentNotices says for every
// consent text whether its purpose is granted once for the account (none)
// or per event registration (registration: the consents page shows no
// grant button, a grant without scope_id is consent.scope_required).
// Other kinds carry no scope type.
func TestCurrentNotices_ScopeType(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t)
	e.publish(t, privacyDraft(consent.AudienceAll, consent.LocaleTR, "2026.10.01"))
	e.consentText(t, consent.PurposeContactInfoShare, "2026.10.01")
	e.consentText(t, consent.PurposeEventSponsor, "2026.10.01")
	rows, err := e.svc.ListCurrentNotices(ctx, authz.Anonymous(), consent.NoticeFilter{Audience: consent.AudienceStudent, Locale: consent.LocaleTR})
	if err != nil {
		t.Fatal(err)
	}
	got := map[consent.Purpose]consent.ScopeType{}
	for _, n := range rows {
		switch {
		case n.Kind != consent.KindConsentText && n.ScopeType != nil:
			t.Fatalf("%s notice carries scope_type %q", n.Kind, *n.ScopeType)
		case n.Kind == consent.KindConsentText && n.ScopeType == nil:
			t.Fatalf("consent text of %s without scope_type", *n.PurposeCode)
		case n.Kind == consent.KindConsentText:
			got[*n.PurposeCode] = *n.ScopeType
		}
	}
	for p, want := range map[consent.Purpose]consent.ScopeType{
		consent.PurposeContactInfoShare: consent.ScopeNone, consent.PurposeEventSponsor: consent.ScopeRegistration,
	} {
		if got[p] != want {
			t.Errorf("%s scope_type = %q, want %q", p, got[p], want)
		}
	}
}
