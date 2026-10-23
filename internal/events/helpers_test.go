package events_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// start is a future start time of the test events.
var start = time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)

// auditLog records the audit events; fail makes Log fail.
type auditLog struct {
	mu     sync.Mutex
	events []audit.Event
	fail   error
}

func (l *auditLog) Log(_ context.Context, e audit.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return l.fail
	}
	l.events = append(l.events, e)
	return nil
}

func (l *auditLog) of(action authz.Action) []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []audit.Event
	for _, e := range l.events {
		if e.Action == string(action) {
			out = append(out, e)
		}
	}
	return out
}

func (l *auditLog) setFail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail = err
}

// txKey marks the context of an open spyTx transaction.
type txKey struct{}

// spyTx is db.NopTx that numbers its transactions in the context, so a
// test can tell which calls share one.
type spyTx struct {
	mu sync.Mutex
	n  int
}

func (s *spyTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if ctx.Value(txKey{}) != nil {
		return fn(ctx)
	}
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	return db.NopTx{}.WithTx(ctx, func(inner context.Context) error {
		return fn(context.WithValue(inner, txKey{}, n))
	})
}

func (s *spyTx) WithSerializableTx(ctx context.Context, fn func(context.Context) error) error {
	return s.WithTx(ctx, fn)
}

func txOf(ctx context.Context) int {
	n, _ := ctx.Value(txKey{}).(int)
	return n
}

// spyOutbox records the transaction of each Add.
type spyOutbox struct {
	*outbox.Mem
	mu  sync.Mutex
	txs map[string][]int
}

func (s *spyOutbox) Add(ctx context.Context, e domainevent.Envelope) error {
	s.mu.Lock()
	s.txs[e.Topic] = append(s.txs[e.Topic], txOf(ctx))
	s.mu.Unlock()
	return s.Mem.Add(ctx, e)
}

// spyStore records the transaction of each UpdateEvent.
type spyStore struct {
	*events.MemStore
	mu      sync.Mutex
	updates []int
}

func (s *spyStore) UpdateEvent(ctx context.Context, e events.Event) (events.Event, error) {
	s.mu.Lock()
	s.updates = append(s.updates, txOf(ctx))
	s.mu.Unlock()
	return s.MemStore.UpdateEvent(ctx, e)
}

// fakeOrgs is the organiser directory of the authztest orgs.
type fakeOrgs struct {
	mu   sync.Mutex
	orgs map[uuid.UUID]orgs.OrgSummary
}

func newFakeOrgs() *fakeOrgs {
	f := &fakeOrgs{orgs: map[uuid.UUID]orgs.OrgSummary{}}
	for _, o := range authztest.Orgs() {
		id := authztest.ID(o.Name)
		f.orgs[id] = orgs.OrgSummary{ID: id, Kind: orgs.Kind(o.Kind), Name: "Org " + o.Name,
			Slug: strings.ReplaceAll(o.Name, "_", "-"), Status: orgs.StatusActive}
	}
	return f
}

func (f *fakeOrgs) Get(_ context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orgs[id]
	if !ok {
		return orgs.OrgSummary{}, orgs.ErrNotFound
	}
	return o, nil
}

func (f *fakeOrgs) GetMany(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]orgs.OrgSummary{}
	for _, id := range ids {
		if o, ok := f.orgs[id]; ok {
			out[id] = o
		}
	}
	return out, nil
}

// ActiveMemberIDs: the personas and helpers with a membership of id.
func (f *fakeOrgs) ActiveMemberIDs(_ context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.Actor.MemberOf(id) {
			out = append(out, p.User())
		}
	}
	return out, nil
}

// HasRole: the persona's membership roles.
func (f *fakeOrgs) HasRole(_ context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error) {
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.User() == userID && p.Actor.HasOrgRole(orgID, roles...) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeOrgs) setStatus(id uuid.UUID, st orgs.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orgs[id]
	o.Status = st
	f.orgs[id] = o
}

// fakeCatalog is directory for the authztest orgs: club_sensitive is a
// sensitive club; every club and university org belongs to its university.
type fakeCatalog struct{}

func (fakeCatalog) SensitiveOrgIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if id == authztest.ID("club_sensitive") {
			out[id] = true
		}
	}
	return out, nil
}

func (fakeCatalog) GetClub(_ context.Context, id uuid.UUID) (directory.Club, error) {
	for _, o := range authztest.Orgs() {
		if o.Kind == authz.OrgClub && authztest.ID(o.Name) == id {
			return directory.Club{OrgID: id, UniversityID: authztest.UniversityID(o.University),
				IsSensitive: o.Sensitive}, nil
		}
	}
	return directory.Club{}, directory.ErrNotFound
}

func (fakeCatalog) GetUniversity(_ context.Context, id uuid.UUID) (directory.University, error) {
	for _, n := range []string{"uni_a", "uni_b"} {
		if authztest.UniversityID(n) == id {
			return directory.University{ID: id, OrgID: authztest.ID(n), Name: "Uni " + n, IsActive: true}, nil
		}
	}
	return directory.University{}, directory.ErrNotFound
}

func (fakeCatalog) GetDepartment(context.Context, uuid.UUID) (directory.Department, error) {
	return directory.Department{}, directory.ErrNotFound
}

func (fakeCatalog) UniversityOrgs(_ context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	for _, o := range authztest.Orgs() {
		switch {
		case o.Kind == authz.OrgUniversity && authztest.UniversityID(o.Name) == id,
			o.Kind == authz.OrgClub && authztest.UniversityID(o.University) == id:
			out = append(out, authztest.ID(o.Name))
		}
	}
	return out, nil
}

// fakeUsers is identity's user directory: an account per persona,
// "<name>@example.test", last name "Yılmaz". Contact (BE-031) answers for
// every account that is not erased.
type fakeUsers struct{ suspended, erased map[uuid.UUID]bool }

// Contact implements events.Contacts: "u<8 hex>@example.test", first name
// Ayla, locale tr.
func (u fakeUsers) Contact(_ context.Context, id uuid.UUID) (identity.UserContact, error) {
	if u.erased[id] {
		return identity.UserContact{}, identity.ErrNotFound
	}
	return identity.UserContact{ID: id, Email: "u" + id.String()[:8] + "@example.test", FirstName: "Ayla", LastName: "Yılmaz",
		Locale: "tr"}, nil
}

func (u fakeUsers) Get(_ context.Context, id uuid.UUID) (identity.UserSummary, error) {
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.Actor.IsUser() && p.User() == id {
			st := identity.StatusActive
			if u.suspended[id] {
				st = identity.StatusSuspended
			}
			return identity.UserSummary{ID: id, Status: st, FirstName: "Ayla", LastName: "Yılmaz"}, nil
		}
	}
	return identity.UserSummary{}, identity.ErrNotFound
}

func (u fakeUsers) GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error) {
	out := map[uuid.UUID]identity.UserSummary{}
	for _, id := range ids {
		if s, err := u.Get(ctx, id); err == nil {
			out[id] = s
		}
	}
	return out, nil
}

func (u fakeUsers) FindByEmail(_ context.Context, email string) (uuid.UUID, bool, error) {
	name, ok := strings.CutSuffix(strings.ToLower(strings.TrimSpace(email)), "@example.test")
	if !ok {
		return uuid.Nil(), false, nil
	}
	p, ok := authztest.Get(name)
	if !ok || !p.Actor.IsUser() {
		return uuid.Nil(), false, nil
	}
	return p.User(), true, nil
}

// fakeNotices is consent's current consent texts: every purpose has one
// unless it is listed in missing.
type fakeNotices struct {
	mu      sync.Mutex
	missing map[consent.Purpose]bool
}

func (f *fakeNotices) Current(_ context.Context, key consent.NoticeKey) (consent.Notice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key.Kind != consent.KindConsentText || key.Purpose == nil || f.missing[*key.Purpose] {
		return consent.Notice{}, consent.ErrNoNotice
	}
	body := "Katıldığım etkinliğin sponsoruyla bilgilerimin paylaşılmasına izin veriyorum."
	sum := sha256.Sum256([]byte(body))
	pub := t0
	p := *key.Purpose
	return consent.Notice{
		ID: authztest.ID("notice_" + string(p)), Kind: consent.KindConsentText, PurposeCode: &p, Audience: consent.AudienceAll,
		Locale: consent.LocaleTR, Version: "2026.10.02", Title: "Sponsorla paylaşım açık rıza metni", BodyMD: body,
		BodySHA256: sum[:], PublishedAt: &pub,
	}, nil
}

func (f *fakeNotices) setMissing(p consent.Purpose) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missing[p] = true
}

// spyRecorder is consent.Recorder: it records each consent with the
// transaction it was written in.
type spyRecorder struct {
	mu   sync.Mutex
	recs []consent.RecordInput
	txs  []int
	fail error
}

func (r *spyRecorder) Record(ctx context.Context, in consent.RecordInput) (uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return uuid.Nil(), r.fail
	}
	r.recs = append(r.recs, in)
	r.txs = append(r.txs, txOf(ctx))
	return uuid.NewV7(), nil
}

func (r *spyRecorder) records() ([]consent.RecordInput, []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.recs), slices.Clone(r.txs)
}

// spySender is notify: it records every message; e-mails also go through
// a real notify service (mail, BE-031) over an in-memory transport, so its
// validation (CID references, attachment types) and its dedupe keys apply.
type spySender struct {
	mu   sync.Mutex
	msgs []notify.Message
	mail notify.Sender
}

func (s *spySender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
	if s.mail != nil && m.Channel == notify.ChannelEmail {
		return s.mail.Send(ctx, m)
	}
	return uuid.NewV7(), nil
}

// emails returns the e-mail messages of template tmpl ("": all), one per
// dedupe key (a repeated Send of a key is the same message).
func (s *spySender) emails(tmpl string) []notify.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []notify.Message
	seen := map[string]bool{}
	for _, m := range s.msgs {
		if m.Channel == notify.ChannelEmail && (tmpl == "" || m.Template == tmpl) && !seen[m.DedupeKey] {
			seen[m.DedupeKey] = true
			out = append(out, m)
		}
	}
	return out
}

// embeddedTemplates are notify's templates, parsed once.
var embeddedTemplates = sync.OnceValues(notify.EmbeddedTemplates)

// hashIndex is a blind index stand-in (notify.RecipientIndexer).
type hashIndex struct{}

func (hashIndex) Index(email string) []byte { h := sha256.Sum256([]byte(email)); return h[:] }

// realMail is a notify service over an in-memory store and transport.
func realMail(t *testing.T, clk *clock.Fake) (notify.Sender, *notify.MemTransport, notify.Store) {
	t.Helper()
	tpl, err := embeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	tr, st := notify.NewMemTransport(), notify.NewMemStore()
	mod := notify.New(notify.Deps{Store: st, Tx: db.NopTx{}, Transport: tr, Templates: tpl, Clock: clk,
		Log: obs.Discard(), FromAddress: "noreply@koro.test", FromName: "Koro", Recipients: hashIndex{}, Audit: &auditLog{}})
	return mod.Service, tr, st
}

func (s *spySender) sent() []notify.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs)
}

// testEnvelope seals the sensitive answers (a fixed local KEK).
var testEnvelope = sync.OnceValue(func() *crypto.Envelope {
	kek, err := crypto.NewLocalKEK([]crypto.Key{{ID: "test1", Secret: []byte("events-test-kek-32-bytes-long!!!")}})
	if err != nil {
		panic(err)
	}
	env, err := crypto.NewEnvelope(kek)
	if err != nil {
		panic(err)
	}
	return env
})

// testDeriver derives the ticket tokens.
var testDeriver = sync.OnceValue(func() *crypto.Deriver {
	d, err := crypto.NewDeriver([]crypto.Key{{ID: "t1", Secret: []byte("events-test-ticket-key-events-test-ticket-key")}})
	if err != nil {
		panic(err)
	}
	return d
})

type env struct {
	notices *fakeNotices
	consent *spyRecorder
	sender  *spySender
	mailbox *notify.MemTransport // the e-mails notify handed to the transport
	prefs   notify.Store         // notify's store (notification preferences)
	queue   *queue.Mem
	clk     *clock.Fake
	tx      *spyTx
	mem     *events.MemStore
	store   *spyStore
	out     *spyOutbox
	audit   *auditLog
	orgs    *fakeOrgs
	users   fakeUsers
	files   files.Service                               // the real files service over in-memory storage (BE-050)
	objects *storage.Mem                                // its object storage
	reauth  *audittest.LateReauth[identity.ReauthProof] // reauth{} with an overload switch (BE-050)
	destr   *spyDestructions                            // kvkk's destruction record (BE-083, T-318)
	svc     events.Service
	mod     events.Module
}

// goodPassword is the re-authentication password every test user has.
const goodPassword = "dogru-parola-12345"

// reauth is identity's Verify for the tests: goodPassword passes, no
// password is reauth_required, anything else reauth_failed.
type reauth struct{}

func (reauth) Verify(_ context.Context, _ uuid.UUID, p identity.ReauthProof) (string, error) {
	switch p.Password {
	case "":
		return "", identity.ErrReauthRequired
	case goodPassword:
		return "password", nil
	}
	return "", identity.ErrReauthFailed
}

func newEnv(t *testing.T, opts ...func(*events.Deps)) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{
		clk: clk, tx: &spyTx{}, mem: events.NewMemStore(clk.Now), audit: &auditLog{}, orgs: newFakeOrgs(),
		out:     &spyOutbox{Mem: outbox.NewMem(clk), txs: map[string][]int{}},
		users:   fakeUsers{suspended: map[uuid.UUID]bool{}, erased: map[uuid.UUID]bool{}},
		notices: &fakeNotices{missing: map[consent.Purpose]bool{}}, consent: &spyRecorder{}, sender: &spySender{},
		queue: queue.NewMem(), objects: storage.NewMem(clk), reauth: &audittest.LateReauth[identity.ReauthProof]{Next: reauth{}},
		destr: &spyDestructions{},
	}
	e.sender.mail, e.mailbox, e.prefs = realMail(t, clk)
	e.store = &spyStore{MemStore: e.mem}
	idem := idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})
	fm, err := files.New(files.Deps{
		Store: files.NewMemStore(clk.Now), Objects: e.objects, Tx: e.tx, Outbox: e.out, Queue: queue.NewMem(),
		Idempotency: idem, Clock: clk, Log: obs.Discard(), Buckets: files.Buckets{Files: "files", Quarantine: "quarantine"},
		Audit: e.audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.files = fm.Service
	d := events.Deps{
		Store: e.store, Tx: e.tx, Outbox: e.out, Audit: e.audit, Orgs: e.orgs, Catalog: fakeCatalog{},
		Idempotency: idem, Clock: clk, Log: obs.Discard(), Users: e.users, Notices: e.notices,
		Consents: e.consent, Sealer: testEnvelope(), Tickets: testDeriver(), Sender: e.sender,
		QR: events.NewQRRenderer(), Contacts: e.users, Queue: e.queue, LinkBaseURL: "https://koro.test", ICSOrganizer: "noreply@koro.test",
		Files: e.files, Reauth: e.reauth, Destructions: e.destr,
	}
	for _, o := range opts {
		o(&d)
	}
	mod, err := events.New(d)
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func orgID(name string) uuid.UUID { return authztest.ID(name) }

// input is a valid in-person draft that needs registration.
func input(title string) events.EventInput {
	return events.EventInput{
		Title: title, Category: events.CategoryWorkshop, Format: events.FormatInPerson, City: "İstanbul",
		LocationName: "Mühendislik Fakültesi", StartsAt: start, EndsAt: start.Add(2 * time.Hour),
	}
}

// create makes a draft as actor in org.
func (e *env) create(t *testing.T, actor, org string, in events.EventInput) events.EventView {
	t.Helper()
	v, err := e.svc.CreateEvent(t.Context(), persona(t, actor), orgID(org), in)
	if err != nil {
		t.Fatalf("CreateEvent(%s in %s): %v", actor, org, err)
	}
	return v
}

// published creates and publishes an event of club_a (the form version is
// stood in for, BE-026).
func (e *env) published(t *testing.T, in events.EventInput) events.EventView {
	t.Helper()
	v := e.create(t, "club_admin", "club_a", in)
	if err := e.mem.SetFormVersion(v.Event.ID, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.PublishEvent(t.Context(), persona(t, "club_admin"), v.Event.ID)
	if err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	return p
}

func wantCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	p, ok := errors.AsType[*problem.Error](err)
	if !ok || p.Status() != status || p.Code() != code {
		t.Fatalf("error = %v, want %d %s", err, status, code)
	}
}

func wantNotFound(t *testing.T, err error) { t.Helper(); wantCode(t, err, 404, "not_found") }

func wantForbidden(t *testing.T, err error) { t.Helper(); wantCode(t, err, 403, "forbidden") }

func authzUniversity(name string) uuid.UUID { return authztest.UniversityID(name) }

func compare(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }

func eventJSON(t *testing.T, v events.EventView) string {
	t.Helper()
	b, err := json.Marshal(events.ToEventDTO(v))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func cardJSON(t *testing.T, v events.EventView) string {
	t.Helper()
	b, err := json.Marshal(events.ToCardDTO(v))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
