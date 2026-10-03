package students_test

import (
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/domainevent"
	"koro/internal/students"
)

// proof is a consent text the student saw.
func proof() *students.ConsentProof {
	sum := sha256.Sum256([]byte("Keşif rızası metni"))
	return &students.ConsentProof{NoticeVersionID: uuid.NewV7(), TextSHA256: sum[:]}
}

// TestPrivacy_DefaultsArePrivate (02 §9.2, agenda §4): without a settings
// row every flag is off (level 2 closed, contact hidden, clubs hidden) and
// the ranking choice is default.
func TestPrivacy_DefaultsArePrivate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.student(t, e.uniA, false)
	p, err := e.svc.GetMyPrivacy(t.Context(), a)
	if err != nil || p.Discoverable || p.ShowContactOnGrant || p.ShowClubsOnGrant || p.ActivityRankingChoice != students.RankingDefault {
		t.Fatalf("defaults = %+v, %v", p, err)
	}
	if c, err := e.svc.RankingChoice(t.Context(), a.UserID); err != nil || c != students.RankingDefault {
		t.Fatalf("RankingChoice = %q, %v", c, err)
	}
}

// TestUpdatePrivacy_Minor_Discoverable_422 (03 k9, Y4): a minor can never
// turn discoverability or contact sharing on, with or without a consent
// text; nothing is written. Turning them off and the club and ranking
// settings stay open to minors.
func TestUpdatePrivacy_Minor_Discoverable_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	m := e.student(t, e.uniA, true)
	for name, patch := range map[string]students.PrivacyPatch{
		"discoverable": {Discoverable: ptr(true), DiscoveryConsent: proof()},
		"contact":      {ShowContactOnGrant: ptr(true), ContactConsent: proof()},
		"both":         {Discoverable: ptr(true), ShowContactOnGrant: ptr(true), ShowClubsOnGrant: ptr(true)},
	} {
		_, err := e.svc.UpdateMyPrivacy(ctx, m, patch)
		if !errors.Is(err, students.ErrMinorRestricted) {
			t.Fatalf("%s: %v", name, err)
		}
		wantCode(t, err, "students.minor_restricted")
	}
	if len(e.consent.records()) != 0 {
		t.Fatal("a consent was recorded for a minor's refused grant")
	}
	if _, err := e.store.GetPrivacy(ctx, m.UserID); !errors.Is(err, students.ErrNotFound) {
		t.Fatal("settings written for a refused request")
	}
	got, err := e.svc.UpdateMyPrivacy(ctx, m, students.PrivacyPatch{Discoverable: ptr(false), ShowClubsOnGrant: ptr(true),
		ActivityRankingChoice: ptr(students.RankingDisabled)})
	if err != nil || got.Discoverable || !got.ShowClubsOnGrant || got.ActivityRankingChoice != students.RankingDisabled {
		t.Fatalf("minor's allowed changes = %+v, %v", got, err)
	}
}

// TestUpdatePrivacy_RecordsConsentSameTx (02 §9.2, T-33): turning
// discoverable on records the discovery_profile grant (with the text the
// student saw) in the very transaction that writes the setting; turning it
// off records the withdrawal the same way; a failing consent record fails
// the request and nothing after it (audit, event) is written. The
// PostgreSQL rollback is pinned by TestUpdatePrivacy_ConsentFailureRollsBack_Postgres.
func TestUpdatePrivacy_RecordsConsentSameTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	pr := proof()
	got, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{Discoverable: ptr(true), DiscoveryConsent: pr,
		Channel: consent.ChannelMobile})
	if err != nil || !got.Discoverable {
		t.Fatalf("turn on = %+v, %v", got, err)
	}
	recs := e.consent.records()
	if len(recs) != 1 || recs[0].Purpose != consent.PurposeDiscoveryProfile || recs[0].Action != consent.Granted ||
		recs[0].UserID != a.UserID || *recs[0].NoticeVersionID != pr.NoticeVersionID || recs[0].Channel != consent.ChannelMobile {
		t.Fatalf("consent records = %+v", recs)
	}
	if e.consent.tx[0] == 0 || e.consent.tx[0] != e.store.privacyTx[0] {
		t.Fatalf("consent in tx %d, setting in tx %d: not the same transaction", e.consent.tx[0], e.store.privacyTx[0])
	}
	ev := e.audit.of(string(authz.StudentsPrivacyUpdateOwn))
	if len(ev) != 1 || ev[0].Metadata["discoverable"] != true || ev[0].Metadata["discovery_profile_consent_id"] == nil {
		t.Fatalf("audit = %+v", ev)
	}

	// Off: the withdrawal, same transaction again.
	if _, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{Discoverable: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	recs = e.consent.records()
	if len(recs) != 2 || recs[1].Action != consent.Withdrawn || e.consent.tx[1] != e.store.privacyTx[1] {
		t.Fatalf("withdrawal = %+v (tx %v / %v)", recs, e.consent.tx, e.store.privacyTx)
	}

	// On without the consent text: 422 consent.proof_required.
	_, err = e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{ShowContactOnGrant: ptr(true), ShowClubsOnGrant: ptr(true)})
	wantCode(t, err, "consent.proof_required")
	if pr, _ := e.svc.GetMyPrivacy(ctx, a); pr.ShowContactOnGrant || pr.ShowClubsOnGrant {
		t.Fatalf("a refused change was written: %+v", pr)
	}

	// A failing consent record fails the change; no audit, no event.
	e.consent.fail = errors.New("consent store down")
	auditBefore, topicsBefore := len(e.audit.of(string(authz.StudentsPrivacyUpdateOwn))), len(e.topics())
	if _, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{Discoverable: ptr(true), DiscoveryConsent: proof(),
		ActivityRankingChoice: ptr(students.RankingEnabled)}); err == nil {
		t.Fatal("the change succeeded without its consent record")
	}
	if len(e.audit.of(string(authz.StudentsPrivacyUpdateOwn))) != auditBefore || len(e.topics()) != topicsBefore {
		t.Fatal("audit or event written for a failed change")
	}
}

// TestUpdatePrivacy_RankingChoiceEventAndClubsFlag: the ranking choice
// writes student.ranking_choice_changed (O) in the transaction; the club
// flag needs no consent; an unchanged value writes nothing.
func TestUpdatePrivacy_RankingChoiceEventAndClubsFlag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	if _, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{ActivityRankingChoice: ptr(students.RankingDisabled),
		ShowClubsOnGrant: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	evs := e.out.Events()
	if len(evs) != 1 || evs[0].Topic != domainevent.TopicStudentRankingChoiceChanged {
		t.Fatalf("events = %v", e.topics())
	}
	p, err := domainevent.Decode[domainevent.StudentRankingChoiceChanged](evs[0])
	if err != nil || p.UserID != a.UserID || p.Choice != "disabled" {
		t.Fatalf("payload = %+v, %v", p, err)
	}
	if len(e.consent.records()) != 0 {
		t.Fatal("the clubs flag or the ranking choice recorded a consent")
	}
	if _, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{ActivityRankingChoice: ptr(students.RankingDisabled)}); err != nil {
		t.Fatal(err)
	}
	if len(e.out.Events()) != 1 || len(e.audit.of(string(authz.StudentsPrivacyUpdateOwn))) != 1 {
		t.Fatal("an unchanged setting was written again")
	}
	_, err = e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{})
	wantCode(t, err, "students.empty_patch")
	if c, _ := e.svc.RankingChoice(ctx, a.UserID); c != students.RankingDisabled {
		t.Fatalf("RankingChoice = %q", c)
	}
	if !slices.Contains(e.topics(), domainevent.TopicStudentRankingChoiceChanged) {
		t.Fatal("event lost")
	}
}
