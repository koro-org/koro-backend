package outreach_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/entitlements"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/outreach"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
)

// ---- dispatch fakes (BE-035)

// fakeUsers is identity: accounts (active unless suspended, adult unless
// minor) and their addresses.
type fakeUsers struct {
	mu    sync.Mutex
	users map[uuid.UUID]identity.UserSummary
	err   error
}

func (f *fakeUsers) add(minor bool, ids ...uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.users[id] = identity.UserSummary{ID: id, Status: identity.StatusActive, FirstName: "Ada", LastName: "Yılmaz",
			Locale: "tr-TR", IsMinor: minor}
	}
}

func (f *fakeUsers) suspend(id uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[id]
	u.Status = identity.StatusSuspended
	f.users[id] = u
}

func (f *fakeUsers) GetMany(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := map[uuid.UUID]identity.UserSummary{}
	for _, id := range ids {
		if u, ok := f.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (f *fakeUsers) Contact(_ context.Context, id uuid.UUID) (identity.UserContact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return identity.UserContact{}, identity.ErrNotFound
	}
	return identity.UserContact{ID: id, Email: id.String()[:8] + "@example.test", Locale: u.Locale}, nil
}

// fakeConsents is consent's FilterActive over grants per purpose.
type fakeConsents struct {
	mu     sync.Mutex
	grants map[consent.Purpose]map[uuid.UUID]bool
	calls  int
}

func (f *fakeConsents) grant(p consent.Purpose, ids ...uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grants[p] == nil {
		f.grants[p] = map[uuid.UUID]bool{}
	}
	for _, id := range ids {
		f.grants[p][id] = true
	}
}

func (f *fakeConsents) FilterActive(_ context.Context, ids []uuid.UUID, p consent.Purpose) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	var out []uuid.UUID
	for _, id := range ids {
		if f.grants[p][id] {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, uuid.UUID.Compare)
	return slices.Compact(out), nil
}

func (f *fakeConsents) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeMembers is orgs' active member lists.
type fakeMembers struct {
	mu      sync.Mutex
	members map[uuid.UUID][]uuid.UUID
}

func (f *fakeMembers) set(org uuid.UUID, ids ...uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[org] = append(f.members[org], ids...)
}

func (f *fakeMembers) ActiveMemberIDs(_ context.Context, org uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.members[org]), nil
}

// fakeSender is notify with its dedupe semantics (02 §6.1
// notification_deliveries_dedupe_key_uq): the first Send of a key
// delivers, a later one returns the same delivery without delivering
// again. failAfter > 0 makes the Send after that many deliveries fail with
// failErr (a crash in the middle of a run).
type fakeSender struct {
	mu        sync.Mutex
	byKey     map[string]uuid.UUID
	delivered []notify.Message
	attempts  int
	failAfter int
	failErr   error
}

func (f *fakeSender) Send(_ context.Context, m notify.Message) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if id, ok := f.byKey[m.DedupeKey]; ok {
		return id, nil
	}
	if f.failAfter > 0 && len(f.delivered) >= f.failAfter {
		f.failAfter = 0
		return uuid.Nil(), f.failErr
	}
	id := uuid.NewV7()
	f.byKey[m.DedupeKey] = id
	f.delivered = append(f.delivered, m)
	return id, nil
}

// to returns the users notified on channel ch (each once per delivery).
func (f *fakeSender) to(ch notify.Channel) []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for _, m := range f.delivered {
		if m.Channel == ch {
			out = append(out, *m.UserID)
		}
	}
	return out
}

func (f *fakeSender) messages() []notify.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.delivered)
}

// fakePosts is the feed's view of posts.
type fakePosts struct {
	mu    sync.Mutex
	posts map[uuid.UUID]outreach.PostInfo
}

func (f *fakePosts) put(p outreach.PostInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts[p.ID] = p
}

func (f *fakePosts) setStatus(id uuid.UUID, st outreach.PostStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.posts[id]
	p.Status = st
	f.posts[id] = p
}

func (f *fakePosts) DispatchPost(_ context.Context, id uuid.UUID) (outreach.PostInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.posts[id]
	if !ok {
		return outreach.PostInfo{}, outreach.ErrNotFound
	}
	return p, nil
}

// ---- dispatch helpers

const testUnsubscribe = "https://koro.example.test/unsubscribe"

// fakeLinks mints one link per recipient and purpose, as consent does
// (BE-062): the base, the purpose and the user ID; fail makes it refuse.
type fakeLinks struct{ fail error }

func (f fakeLinks) UnsubscribeURL(userID uuid.UUID, purpose consent.Purpose) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	return linkOf(userID, purpose), nil
}

func linkOf(userID uuid.UUID, purpose consent.Purpose) string {
	return testUnsubscribe + "/" + string(purpose) + "-" + userID.String()
}

// targetedPost puts a published targeted post of org (author kind) for keys.
func (e *env) targetedPost(org uuid.UUID, kind outreach.AuthorKind, keys ...string) outreach.PostInfo {
	p := outreach.PostInfo{
		ID: uuid.NewV7(), OrgID: org, AuthorKind: kind, Status: outreach.PostPublished, Targeted: true,
		Keys: keys, DimCount: dims(keys), Title: "Yaz stajı", Commercial: kind == outreach.AuthorCompany,
	}
	e.posts.put(p)
	return p
}

// dims is the number of dimensions of keys.
func dims(keys []string) int16 {
	seen := map[string]bool{}
	for _, k := range keys {
		d, _, _ := strings.Cut(k, ":")
		seen[d] = true
	}
	return int16(len(seen)) // at most 4 dimensions
}

// grantDispatches gives org n targeted_dispatch units.
func (e *env) grantDispatches(t *testing.T, org uuid.UUID, n int) {
	t.Helper()
	by := authztest.ID("platform_admin")
	if _, err := e.ents.Grant(t.Context(), entitlements.GrantInput{
		OrgID: org, Feature: entitlements.FeatureTargetedDispatch, Quantity: &n, ValidFrom: t0.Add(-time.Hour),
		ValidTo: t0.Add(30 * 24 * time.Hour), Status: entitlements.StatusActive, SourceType: entitlements.SourceAdminGrant,
		GrantedBy: &by,
	}); err != nil {
		t.Fatal(err)
	}
}

// units is what org has left of targeted_dispatch.
func (e *env) units(t *testing.T, org uuid.UUID) int {
	t.Helper()
	av, err := e.ents.Available(t.Context(), org, entitlements.FeatureTargetedDispatch)
	if err != nil {
		t.Fatal(err)
	}
	return av.Remaining
}

// schedule schedules a dispatch of p by persona over chans.
func (e *env) schedule(t *testing.T, persona string, p outreach.PostInfo, chans ...outreach.Channel) outreach.Dispatch {
	t.Helper()
	id, err := e.svc.ScheduleDispatch(t.Context(), outreach.DispatchInput{Actor: actor(t, persona), Post: p, Channels: chans})
	if err != nil {
		t.Fatalf("ScheduleDispatch: %v", err)
	}
	d, err := e.store.GetDispatch(t.Context(), p.OrgID, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// deliver runs the outbox events (outreach.dispatch_scheduled) recorded so
// far through outreach.run_dispatch, as the worker would.
func (e *env) deliver(t *testing.T) error {
	t.Helper()
	var errs []error
	for _, env := range e.out.Events() {
		if env.Topic != domainevent.TopicOutreachDispatchScheduled || e.delivered[env.ID] {
			continue
		}
		e.delivered[env.ID] = true
		for _, s := range e.subs.For(env.Topic) {
			if err := s.Handle(t.Context(), env); err != nil {
				errs = append(errs, err)
				e.delivered[env.ID] = false // retried by the next deliver (asynq)
			}
		}
	}
	return errors.Join(errs...)
}

// run is the worker's run of dispatch id through the event handler.
func (e *env) run(t *testing.T, d outreach.Dispatch) error {
	t.Helper()
	env, err := domainevent.New(domainevent.OutreachDispatchScheduled{DispatchID: d.ID, OrgID: d.OrgID, PostID: d.PostID},
		"outreach_dispatch", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range e.subs.For(env.Topic) {
		if err := s.Handle(t.Context(), env); err != nil {
			return err
		}
	}
	return nil
}

func (e *env) dispatch(t *testing.T, d outreach.Dispatch) outreach.Dispatch {
	t.Helper()
	got, err := e.store.GetDispatch(t.Context(), d.OrgID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// withKey is ctx with an Idempotency-Key of persona on path.
func withKey(ctx context.Context, a authz.Actor, path, key string) context.Context {
	return idempotency.WithKey(ctx, idempotency.Key{Scope: a.UserID.String() + ":POST " + path, Value: key, Status: 201})
}
