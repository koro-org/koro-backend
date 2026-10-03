package kvkk_test

import (
	"slices"
	"strings"
	"testing"

	"koro/internal/authz"
	"koro/internal/kvkk"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/privacy"
)

// erasureRequest creates and answers student's erasure request; the
// dsr.erasure_approved event is written, not delivered.
func erasureRequest(t *testing.T, e *env) kvkk.DSR {
	t.Helper()
	d, err := e.svc.CreateMine(t.Context(), actor(t, "student"), kvkk.MyRequest{Type: kvkk.TypeErasure, Proof: proof})
	if err != nil {
		t.Fatal(err)
	}
	c := e.completeAs(t, d.ID)
	if c.Status != kvkk.StatusCompleted {
		t.Fatalf("answered erasure = %+v", c)
	}
	return c
}

func eraseCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if name, ok := strings.CutPrefix(c, "erase:"); ok {
			out = append(out, name)
		}
	}
	return out
}

// TestErasure_EachProviderOwnTx_IdempotentRerun (01 §6.3): kvkk never wraps
// a provider in its transaction; a provider failure fails the run, the
// redelivered event runs every (idempotent) provider again, and a run that
// finished is not repeated.
func TestErasure_EachProviderOwnTx_IdempotentRerun(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	events := e.providers[0].(*fakeProvider)
	events.fail = true
	d := erasureRequest(t, e)
	if !slices.Contains(e.topics(), domainevent.TopicDSRErasureApproved) || slices.Contains(e.topics(), domainevent.TopicDSRCompleted) {
		t.Fatalf("topics after the answer = %v", e.topics())
	}
	if err := e.deliverTopic(t, domainevent.TopicDSRErasureApproved); err == nil {
		t.Fatal("a failing provider did not fail the run")
	}
	if got, _ := e.store.Get(t.Context(), d.ID); strings.Contains(got.ResponseSummary, "Hesap silme raporu") {
		t.Fatal("a failed run reported the erasure as done")
	}
	if e.users.erased[actor(t, "student").UserID] {
		t.Fatal("identity anonymised although an earlier provider failed")
	}
	// Retry: every provider again, identity included.
	if err := e.deliverTopic(t, domainevent.TopicDSRErasureApproved); err != nil {
		t.Fatal(err)
	}
	for _, p := range e.providers {
		if fp, ok := p.(*fakeProvider); ok && fp.inTx {
			t.Fatalf("provider %s ran inside a kvkk transaction", fp.name)
		}
	}
	calls := len(e.log.all())
	// At-least-once: the finished run is not repeated.
	if err := e.deliverTopic(t, domainevent.TopicDSRErasureApproved); err != nil {
		t.Fatal(err)
	}
	if len(e.log.all()) != calls {
		t.Fatalf("a finished erasure ran again: %v", e.log.all()[calls:])
	}
	done := 0
	for _, ev := range e.out.Events() {
		if ev.Topic == domainevent.TopicDSRCompleted {
			done++
			if p := decodeCompleted(t, ev); p.Outcome != kvkk.OutcomeErased || p.DSRID != d.ID {
				t.Fatalf("dsr.completed = %+v", p)
			}
		}
	}
	if done != 1 {
		t.Fatalf("dsr.completed written %d times", done)
	}
	if a := e.audit.of(string(authz.KvkkDsrComplete)); a[len(a)-1].Metadata["step"] != "erasure_done" ||
		a[len(a)-1].SystemJob != "kvkk.erase_account" {
		t.Fatalf("erasure audit = %+v", a[len(a)-1])
	}
}

// TestErasure_IdentityAnonymizedLast (01 §6.3): every other provider erases
// before identity; after identity the others run once more (sweep). The
// subject is mailed before the address disappears. Sessions: see
// cmd/koro TestErasure_IdentityAnonymizedLast_SessionsRevoked (real
// identity).
func TestErasure_IdentityAnonymizedLast(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	d := erasureRequest(t, e)
	if _, err := e.deliver(t, 0); err != nil {
		t.Fatal(err)
	}
	calls := eraseCalls(e.log.all())
	idx := slices.Index(calls, kvkk.IdentityProvider)
	if idx < 0 {
		t.Fatalf("identity never erased: %v", calls)
	}
	first, sweep := calls[:idx], calls[idx+1:]
	slices.Sort(first)
	slices.Sort(sweep)
	others := []string{"consent", "events"} // the recording fakes (kvkk's own provider runs too)
	if !slices.Equal(first, others) || !slices.Equal(sweep, others) {
		t.Fatalf("erase order %v: want %v, then identity, then %v again", calls, others, others)
	}
	if !e.users.erased[actor(t, "student").UserID] {
		t.Fatal("identity did not anonymise the account")
	}
	mails := e.mail.of("kvkk.dsr_completed")
	if len(mails) != 1 || mails[0].Data["outcome"] != kvkk.OutcomeErased || !strings.HasPrefix(mails[0].ToEmail, "kisi-") {
		t.Fatalf("erasure mail = %+v", mails)
	}
	// The other open request of the subject is closed; the rows stay.
	got, _ := e.store.Get(t.Context(), d.ID)
	if !strings.Contains(got.ResponseSummary, "Hesap silme raporu") || !strings.HasPrefix(got.ResponseSummary, "Başvurunuz yanıtlandı.") {
		t.Fatalf("answer after the run = %q", got.ResponseSummary)
	}
}

// TestErasure_FinancialRecordsKeepTombstoneFK (02 §0.6, 03 k31): a module
// that must keep records (financial, 10 years) reports them as retained;
// the account becomes a tombstone (it still exists, so the foreign keys of
// the kept rows stay valid), the report lists the kept records with their
// legal basis, and the request row keeps pointing at the tombstone.
func TestErasure_FinancialRecordsKeepTombstoneFK(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.providers = append(e.providers, &fakeProvider{name: "orders", log: e.log, retained: 2,
		reason: "orders: financial records (TTK md. 82, VUK; 10 years)"})
	student := actor(t, "student")
	open, err := e.svc.CreateMine(t.Context(), student, kvkk.MyRequest{Type: kvkk.TypeObjection})
	if err != nil {
		t.Fatal(err)
	}
	d := erasureRequest(t, e)
	if _, err := e.deliver(t, 0); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.users.Exists(t.Context(), student.UserID); !ok {
		t.Fatal("the account row is gone: the retained records' foreign keys would break")
	}
	if _, live, _ := e.users.Contact(t.Context(), student.UserID); live {
		t.Fatal("the account is not anonymised")
	}
	got, _ := e.store.Get(t.Context(), d.ID)
	if got.UserID == nil || *got.UserID != student.UserID {
		t.Fatal("the request lost its subject")
	}
	for _, want := range []string{"orders: 2 kayıt — orders: financial records", "consent: 1 kayıt", "kvkk: 2 kayıt"} {
		if !strings.Contains(got.ResponseSummary, want) {
			t.Fatalf("report %q lacks %q", got.ResponseSummary, want)
		}
	}
	if o, _ := e.store.Get(t.Context(), open.ID); o.Status != kvkk.StatusCancelled {
		t.Fatalf("the subject's other open request is %s, want cancelled", o.Status)
	}
	audit := e.audit.of(string(authz.KvkkDsrComplete))
	last := audit[len(audit)-1]
	if last.Metadata["retained"] != 1+2+2 || !strings.Contains(last.Metadata["modules"].(string), "orders") {
		t.Fatalf("erasure audit = %+v", last.Metadata)
	}
}

// TestErasure_WithoutIdentityProviderFailsClosed: an erasure never
// "succeeds" while the account itself cannot be anonymised.
func TestErasure_WithoutIdentityProviderFailsClosed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.providers = slices.DeleteFunc(e.providers, func(p privacy.Provider) bool { return p.Name() == kvkk.IdentityProvider })
	erasureRequest(t, e)
	if err := e.deliverTopic(t, domainevent.TopicDSRErasureApproved); err == nil {
		t.Fatal("erasure without identity reported success")
	}
	if slices.Contains(e.topics(), domainevent.TopicDSRCompleted) {
		t.Fatal("dsr.completed written for an unfinished erasure")
	}
}
