package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
)

// matrixReauthPassword is the password the test re-authentication verifier
// accepts (bodies/consent/*.json carry it in "reauth").
const matrixReauthPassword = "matrix-reauth-password"

// errTestReauth is the answer of the test verifier to a wrong or missing
// proof. identity's own codes (identity.reauth_required/_failed) are
// declared by identity (BE-014), so the fake uses the generic 403.
var errTestReauth = problem.ErrForbidden.Wrap(errors.New("test reauth: wrong or missing proof"))

// testReauth is the re-authentication verifier of wiring and matrix tests.
func testReauth(_ context.Context, _ uuid.UUID, password, _ string) (string, error) {
	if subtle.ConstantTimeCompare([]byte(password), []byte(matrixReauthPassword)) != 1 {
		return "", errTestReauth
	}
	return "password", nil
}

// useMemStore installs the in-memory consent store used by wiring and authz
// matrix tests (05 §13.2), a recording İYS client and the test
// re-authentication verifier. First owner: BE-012. Only tickets of the
// consent module edit this file (BE-001 R4).
func (w *consentWiring) useMemStore() {
	w.store = consent.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
	w.iys = &consent.FakeIYS{}
	w.cfg = &config.Consent{}
	w.reauth = testReauth
}

// TestWire_ConsentTasksAndPrivacy pins what consent registers (01 §6.2,
// §7): the İYS subscription on the critical queue, the reconcile job and
// the KVKK provider.
func TestWire_ConsentTasksAndPrivacy(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "consent.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"consent.iys_reconcile 30 5 * * * default"}; !slices.Equal(jobs, want) {
		t.Fatalf("consent jobs %q, want %q", jobs, want)
	}
	subs := c.infra.subs.For(domainevent.TopicConsentRecorded)
	if len(subs) != 1 || subs[0].Name != consent.SubmitIYSTaskName || subs[0].Queue != "critical" {
		t.Fatalf("consent.recorded subscriptions %+v", subs)
	}
	mux, err := c.workerMux(obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{consent.SubmitIYSTaskName, consent.ReconcileJobName} {
		if !slices.Contains(mux.Types(), name) {
			t.Errorf("worker does not route %s", name)
		}
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "consent" }) {
		t.Fatal("consent privacy provider not registered")
	}
}

// TestWire_ConsentRefusesIYSRequiredWithoutClient: CONSENT_IYS_REQUIRED=true
// with the no-op İYS client stops the process at start.
func TestWire_ConsentRefusesIYSRequiredWithoutClient(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.consent.iys = nil
	c.consent.cfg = &config.Consent{IYSRequired: true}
	if err := c.build(); err == nil || !strings.Contains(err.Error(), "CONSENT_IYS_REQUIRED") {
		t.Fatalf("build() error = %v", err)
	}
}

// TestWire_ConsentReauthIsLateBound: without a verifier every R operation
// fails closed; a verifier bound after build (identity, BE-014) is used.
func TestWire_ConsentReauthIsLateBound(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.consent.reauth = nil
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	dpo, _ := authztest.Get("dpo")
	proof := consent.ReauthProof{Password: matrixReauthPassword}
	if _, err := c.consent.svc.UpdatePurpose(t.Context(), dpo.Actor, consent.PurposeCommercialPush,
		consent.PurposePatch{Active: new(false)}, proof); !errors.Is(err, consent.ErrReauthUnavailable) {
		t.Fatalf("UpdatePurpose without a verifier = %v, want %v", err, consent.ErrReauthUnavailable)
	}
	c.consent.reauth = testReauth // what wire_identity.go does in BE-014
	p, err := c.consent.svc.UpdatePurpose(t.Context(), dpo.Actor, consent.PurposeCommercialPush,
		consent.PurposePatch{Active: new(false)}, proof)
	if err != nil || p.Active {
		t.Fatalf("UpdatePurpose after binding = %+v, %v", p, err)
	}
}
