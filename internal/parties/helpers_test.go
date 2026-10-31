package parties_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

// Test tax identifiers (valid check digits; never real persons).
const (
	vknA    = "1234567890"
	vknB    = "9876543217"
	vknC    = "1111111114"
	vknD    = "2223334447"
	vknE    = "4567890128"
	mersisA = "0123456789012345"
	tcknA   = "10000000146"
	tcknB   = "12345678950"
)

// fakeOrgs is orgs.OrgDirectory over the matrix orgs (authztest.Orgs) plus
// extra ones.
type fakeOrgs struct{ m map[uuid.UUID]orgs.OrgSummary }

func newFakeOrgs() *fakeOrgs {
	f := &fakeOrgs{m: map[uuid.UUID]orgs.OrgSummary{}}
	for _, o := range authztest.Orgs() {
		f.add(o.Name, orgs.Kind(o.Kind), orgs.StatusActive)
	}
	f.add("company_suspended", orgs.KindCompany, orgs.StatusSuspended)
	f.add("company_closed", orgs.KindCompany, orgs.StatusClosed)
	return f
}

func (f *fakeOrgs) add(name string, k orgs.Kind, s orgs.Status) {
	id := authztest.ID(name)
	f.m[id] = orgs.OrgSummary{ID: id, Kind: k, Name: name, Status: s, Slug: name}
}

func (f *fakeOrgs) Get(_ context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	o, ok := f.m[id]
	if !ok {
		return orgs.OrgSummary{}, orgs.ErrNotFound
	}
	return o, nil
}

// ActiveMemberIDs returns the personas and helpers with a membership of
// the org (authztest memberships).
func (f *fakeOrgs) ActiveMemberIDs(_ context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.Actor.MemberOf(orgID) {
			out = append(out, p.User())
		}
	}
	return out, nil
}

// HasRole reports a persona's or helper's role in the org.
func (f *fakeOrgs) HasRole(_ context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error) {
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.User() == userID && p.Actor.HasOrgRole(orgID, roles...) {
			return true, nil
		}
	}
	return false, nil
}

// reauthPassword is the proof fakeReauth accepts.
const reauthPassword = "test-reauth-password"

// fakeReauth is identity's re-authentication: reauthPassword passes, no
// proof is 403 identity.reauth_required, anything else reauth_failed.
type fakeReauth struct {
	mu    sync.Mutex
	calls []uuid.UUID
}

func (f *fakeReauth) Verify(_ context.Context, userID uuid.UUID, p identity.ReauthProof) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, userID)
	f.mu.Unlock()
	switch {
	case p.Password == "" && p.TOTPCode == "":
		return "", identity.ErrReauthRequired
	case p.Password != reauthPassword:
		return "", identity.ErrReauthFailed
	}
	return "password", nil
}

func proof() identity.ReauthProof { return identity.ReauthProof{Password: reauthPassword} }

// fakeSender records the notices.
type fakeSender struct {
	mu   sync.Mutex
	sent []notify.Message
	keys map[string]bool
}

func (f *fakeSender) Send(_ context.Context, m notify.Message) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys == nil {
		f.keys = map[string]bool{}
	}
	if f.keys[m.DedupeKey] {
		return uuid.NewV7(), nil
	}
	f.keys[m.DedupeKey] = true
	f.sent = append(f.sent, m)
	return uuid.NewV7(), nil
}

func (f *fakeSender) all() []notify.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.Message(nil), f.sent...)
}

// fakeUsers resolves every persona to an address of its name.
type fakeUsers struct{}

func (fakeUsers) Contact(_ context.Context, id uuid.UUID) (identity.UserContact, error) {
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.User() == id && p.Actor.IsUser() {
			return identity.UserContact{ID: id, Email: p.Name + "@koro.test", FirstName: p.Name, Locale: "tr"}, nil
		}
	}
	return identity.UserContact{}, identity.ErrNotFound
}

// financeAdmins are the finance admins in effect (finance_admin, finance_admin_2).
func financeAdmins(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{authztest.ID("finance_admin"), authztest.ID("finance_admin_2")}, nil
}

// fakeFiles is the files port: metadata by ID, attachments and the TTLs of
// the presigned downloads.
type fakeFiles struct {
	mu       sync.Mutex
	meta     map[uuid.UUID]files.FileMeta
	attached map[uuid.UUID]files.Owner
	ttls     []time.Duration
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{meta: map[uuid.UUID]files.FileMeta{}, attached: map[uuid.UUID]files.Owner{}}
}

// add registers an active, clean party_document of uploader (and org).
func (f *fakeFiles) add(uploader uuid.UUID, org *uuid.UUID) uuid.UUID {
	return f.addPurpose(files.PurposePartyDocument, uploader, org)
}

// addPurpose registers an active, clean file of purpose by uploader (and org).
func (f *fakeFiles) addPurpose(purpose files.Purpose, uploader uuid.UUID, org *uuid.UUID) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewV7()
	f.meta[id] = files.FileMeta{ID: id, Purpose: purpose, UploadedBy: &uploader, OrgID: org,
		OwnerUserID: &uploader, Status: files.StatusActive, ScanStatus: files.ScanClean, MIME: files.MIMEPDF}
	return id
}

func (f *fakeFiles) set(id uuid.UUID, fn func(*files.FileMeta)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.meta[id]
	fn(&m)
	f.meta[id] = m
}

func (f *fakeFiles) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.meta[id]
	if !ok {
		return files.FileMeta{}, files.ErrNotFound
	}
	return m, nil
}

func (f *fakeFiles) Attach(_ context.Context, a authz.Actor, id uuid.UUID, owner files.Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.meta[id]
	switch {
	case !ok:
		return files.ErrNotFound
	case m.Purpose != owner.Purpose:
		return files.ErrPurposeMismatch
	case !a.IsUser():
		return errors.New("attach without a user actor")
	}
	if cur, ok := f.attached[id]; ok && cur != owner {
		return files.ErrAlreadyAttached
	}
	f.attached[id] = owner
	return nil
}

// States answers the state of each known file (BE-154b; the rule of
// files.FileMeta.State with no scanner required).
func (f *fakeFiles) States(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]files.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]files.State{}
	for _, id := range ids {
		if m, ok := f.meta[id]; ok {
			out[id] = m.State(false)
		}
	}
	return out, nil
}

func (f *fakeFiles) PresignGet(_ context.Context, id uuid.UUID, ttl time.Duration) (files.Download, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.meta[id]
	if !ok {
		return files.Download{}, files.ErrNotFound
	}
	if m.Status != files.StatusActive || m.ScanStatus != files.ScanClean {
		return files.Download{}, files.ErrNotClean
	}
	f.ttls = append(f.ttls, ttl)
	return files.Download{URL: "https://files.koro.test/" + id.String(), ExpiresAt: t0.Add(ttl)}, nil
}

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is a parties module over the in-memory store.
type env struct {
	clk    *clock.Fake
	store  *parties.MemStore
	audit  *audittest.Recorder
	orgs   *fakeOrgs
	files  *fakeFiles
	logs   *syncBuffer
	outbox *outbox.Mem
	queue  *queue.Mem
	reauth *fakeReauth
	late   *audittest.LateReauth[identity.ReauthProof] // reauth with an overload switch
	sender *fakeSender
	svc    parties.Service
	mod    parties.Module
}

type envOpt func(*parties.Deps)

// naturalPersons lifts 06 H-54 (the TCKN path is tested with it).
func naturalPersons(d *parties.Deps) { d.AllowNaturalPersonPayees = true }

func testKeys(t *testing.T) (parties.Sealer, parties.Indexer) {
	t.Helper()
	kek, err := crypto.NewLocalKEK([]crypto.Key{{ID: "test-kek", Secret: bytes.Repeat([]byte{7}, crypto.KeySize)}})
	if err != nil {
		t.Fatal(err)
	}
	envl, err := crypto.NewEnvelope(kek)
	if err != nil {
		t.Fatal(err)
	}
	x, err := crypto.NewIndexer([]byte("koro-parties-test-blind-index-key!"))
	if err != nil {
		t.Fatal(err)
	}
	return envl, x
}

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{clk: clk, store: parties.NewMemStore(clk.Now), audit: &audittest.Recorder{}, orgs: newFakeOrgs(), files: newFakeFiles(),
		logs: &syncBuffer{}, outbox: outbox.NewMem(clk), queue: queue.NewMem(), reauth: &fakeReauth{}, sender: &fakeSender{}}
	e.late = &audittest.LateReauth[identity.ReauthProof]{Next: e.reauth}
	sealer, indexer := testKeys(t)
	tx := db.NopTx{}
	d := parties.Deps{
		Store: e.store, Tx: tx, Audit: e.audit, Orgs: e.orgs, Files: e.files, Clock: clk, Sealer: sealer, Indexer: indexer,
		Outbox: e.outbox, Queue: e.queue, Reauth: e.late, Sender: e.sender, Users: fakeUsers{}, FinanceAdmins: financeAdmins,
		// A plain JSON handler: no obs masks, so what the module logs is checked as is.
		Log:         slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Idempotency: idempotency.New(idempotency.Deps{Tx: tx, Store: idempotency.NewMemStore(), Clock: clk, Log: slog.New(slog.DiscardHandler)}),
	}
	for _, o := range opts {
		o(&d)
	}
	mod, err := parties.New(d)
	if err != nil {
		t.Fatalf("parties.New: %v", err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

func actor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func ctxOf(t *testing.T, a authz.Actor) context.Context { return authz.WithActor(t.Context(), a) }

func ptr[T any](v T) *T { return &v }

func org(name string) uuid.UUID { return authztest.ID(name) }

func addr() *parties.Address {
	return &parties.Address{Line1: "Büyükdere Cad. No: 1", District: "Şişli", City: "İstanbul", PostalCode: "34394"}
}

func buyerInput(vkn string) parties.PartyInput {
	return parties.PartyInput{
		LegalType: parties.LegalCompany, Title: "Örnek Yazılım A.Ş.", TaxOffice: "Zincirlikuyu", TaxNo: vkn,
		MersisNo: mersisA, Address: addr(), Email: "fatura@ornek.test", Phone: "0212 555 11 22",
	}
}

func foundationInput(vkn string) parties.PartyInput {
	return parties.PartyInput{
		Kind: parties.KindPayee, LegalType: parties.LegalFoundation, Title: "Örnek Eğitim Vakfı", TaxOffice: "Beşiktaş",
		TaxNo: vkn, Address: addr(), Email: "vakif@ornek.test",
	}
}

// buyer creates a buyer party of company_a as company_admin.
func (e *env) buyer(t *testing.T, vkn string) parties.PartyView {
	t.Helper()
	a := actor(t, "company_admin")
	v, err := e.svc.CreateBuyerParty(ctxOf(t, a), a, org("company_a"), buyerInput(vkn))
	if err != nil {
		t.Fatalf("CreateBuyerParty: %v", err)
	}
	return v
}

// foundation creates a foundation payee party as finance_admin and links
// it to club_a through a payees row.
func (e *env) foundation(t *testing.T, vkn string) parties.PartyView {
	t.Helper()
	a := actor(t, "finance_admin")
	v, err := e.svc.AdminCreateParty(ctxOf(t, a), a, foundationInput(vkn))
	if err != nil {
		t.Fatalf("AdminCreateParty: %v", err)
	}
	e.store.LinkPayee(uuid.NewV7(), org("club_a"), v.Party.ID, "active")
	return v
}

func (e *env) verify(t *testing.T, id uuid.UUID) parties.PartyView {
	t.Helper()
	a := actor(t, "finance_admin")
	v, err := e.svc.AdminVerifyParty(ctxOf(t, a), a, id, parties.Decision{Decision: parties.StatusVerified})
	if err != nil {
		t.Fatalf("AdminVerifyParty: %v", err)
	}
	return v
}

// ---- HTTP

// newHTTP serves the parties operations of e with the persona resolver and
// the Idempotency-Key middleware (as cmd/koro does); log receives the
// request pipeline's logs.
func newHTTP(t *testing.T, e *env, log *slog.Logger) *httpx.App {
	t.Helper()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	app, err := httpx.New(httpx.Deps{Log: log, Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	parties.Register(app.V1, e.svc)
	return app
}

func call(t *testing.T, app *httpx.App, persona, method, path string, body any, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case nil:
	case string:
		buf.WriteString(b)
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if p, _ := authztest.Get(persona); p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d, want %d: %s", rec.Code, want, rec.Body.String())
	}
}

func orgPath(o string, rest ...string) string {
	return "/api/v1/orgs/" + org(o).String() + "/legal-parties" + strings.Join(rest, "")
}

func idPath(id uuid.UUID) string { return "/" + id.String() }

// noDigits reports whether s contains none of the identifiers (also with
// spaces between groups, as people type them).
func noDigits(t *testing.T, where, s string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		spaced := id[:3] + " " + id[3:]
		if strings.Contains(s, id) || strings.Contains(s, spaced) {
			t.Fatalf("%s holds %q:\n%s", where, id, s)
		}
	}
}

func dump(v any) string { return fmt.Sprintf("%+v", v) }

func dbNop() db.TxManager { return db.NopTx{} }

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// idemCtx carries an Idempotency-Key as the HTTP middleware would.
func idemCtx(t *testing.T, a authz.Actor, key string) context.Context {
	return idempotency.WithKey(ctxOf(t, a), idempotency.Key{Scope: a.UserID.String() + ":POST /test", Value: key, Status: 200})
}
