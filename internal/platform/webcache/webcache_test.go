package webcache_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/webcache"
)

// Test vector of koro-web src/lib/revalidate.test.ts (same secret, body and
// timestamp); the expected signature was computed with node:crypto
// (createHmac) and openssl dgst -hmac, independently of this package.
const (
	vectorSecret = "test-secret-0123456789abcdefghijklmnop"
	vectorTS     = "1800000000"
	vectorBody   = `{"tags":["club:odtu-robotik"]}`
	vectorSig    = "sha256=17ace50cab71d96023cdc3ae6bd890d9df5059b3179a9f32b863158ccfae07c5"
)

var t0 = time.Unix(1_800_000_000, 0).UTC()

func TestSign_MatchesKoroWebVector(t *testing.T) {
	t.Parallel()
	if got := webcache.Sign([]byte(vectorSecret), vectorTS, []byte(vectorBody)); got != vectorSig {
		t.Fatalf("Sign() = %s, want %s", got, vectorSig)
	}
	// The body is signed byte for byte: one more space is another signature.
	if webcache.Sign([]byte(vectorSecret), vectorTS, []byte(vectorBody+" ")) == vectorSig {
		t.Fatal("a changed body kept the signature")
	}
	if webcache.Sign([]byte(vectorSecret+"x"), vectorTS, []byte(vectorBody)) == vectorSig {
		t.Fatal("another secret kept the signature")
	}
}

func TestTags_FollowTheWebContract(t *testing.T) {
	t.Parallel()
	cases := []struct{ kind, slug, want string }{
		{"club", "odtu-robotik", "club:odtu-robotik"},
		{"company", "acme", "company:acme"},
		{"university", "odtu", ""},
		{"platform", "koro", ""},
		{"club", "", ""},
	}
	for _, c := range cases {
		if got := webcache.OrgTag(c.kind, c.slug); got != c.want {
			t.Errorf("OrgTag(%q, %q) = %q, want %q", c.kind, c.slug, got, c.want)
		}
	}
	if got := webcache.EventTag("go-atolyesi-2"); got != "event:go-atolyesi-2" {
		t.Fatalf("EventTag = %q", got)
	}
	// post:<uuid> (BE-149, WEB-040): the canonical lower-case UUID, inside
	// the generic tag pattern of koro-web.
	post := uuid.MustParse("0192F3A4-5B6C-7D8E-9FA0-B1C2D3E4F506")
	if got := webcache.PostTag(post); got != "post:0192f3a4-5b6c-7d8e-9fa0-b1c2d3e4f506" || !webcache.ValidTag(got) {
		t.Fatalf("PostTag = %q", got)
	}
	for _, ok := range []string{"sitemap", "club:a", "event:go-atolyesi", "company:x_y.z/1"} {
		if !webcache.ValidTag(ok) {
			t.Errorf("ValidTag(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "Club A", "club:Ö", ":x", "-x", strings.Repeat("a", 129)} {
		if webcache.ValidTag(bad) {
			t.Errorf("ValidTag(%q) = true", bad)
		}
	}
}

// fakeWeb is koro-web's /api/revalidate: it verifies the request like
// verifySignature does and records the tags.
type fakeWeb struct {
	mu       sync.Mutex
	srv      *httptest.Server
	calls    [][]string
	statuses []int // answers in order; then 200
	bad      []string
}

func newFakeWeb(t *testing.T, statuses ...int) *fakeWeb {
	t.Helper()
	w := &fakeWeb{statuses: statuses}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *fakeWeb) serve(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	ts := r.Header.Get(webcache.TimestampHeader)
	sec, _ := strconv.ParseInt(ts, 10, 64)
	switch {
	case r.Method != http.MethodPost || r.URL.Path != webcache.Path:
		w.bad = append(w.bad, "method/path "+r.Method+" "+r.URL.Path)
	case r.Header.Get("Content-Type") != "application/json":
		w.bad = append(w.bad, "content type "+r.Header.Get("Content-Type"))
	case sec < t0.Unix()-300 || sec > t0.Unix()+300:
		w.bad = append(w.bad, "timestamp "+ts)
	case r.Header.Get(webcache.SignatureHeader) != webcache.Sign([]byte(vectorSecret), ts, raw):
		w.bad = append(w.bad, "signature")
	}
	var b map[string][]string
	if err := json.Unmarshal(raw, &b); err != nil || len(b) != 1 || b["tags"] == nil {
		w.bad = append(w.bad, "body "+string(raw)) // only {"tags": [...]}: nothing else, no personal data
	}
	w.calls = append(w.calls, b["tags"])
	status := http.StatusOK
	if len(w.statuses) > 0 {
		status, w.statuses = w.statuses[0], w.statuses[1:]
	}
	rw.WriteHeader(status)
}

func (w *fakeWeb) got(t *testing.T) [][]string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.bad) > 0 {
		t.Fatalf("koro-web would reject: %v", w.bad)
	}
	return slices.Clone(w.calls)
}

// newSender targets w through w's own client. The default client would share
// http.DefaultTransport with every parallel test, and httptest.Server.Close
// calls DefaultTransport.CloseIdleConnections: another test's server closing
// then breaks this test's request ("CloseIdleConnections called", BE-125).
func newSender(t *testing.T, w *fakeWeb) *webcache.Sender {
	t.Helper()
	s, err := webcache.NewSender(webcache.SenderConfig{
		BaseURL: w.srv.URL + "/", Secret: vectorSecret, Clock: clock.NewFake(t0), Client: w.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSender_PostsSignedTags(t *testing.T) {
	t.Parallel()
	web := newFakeWeb(t)
	tags := []string{"club:odtu-robotik", "event:go-atolyesi", "club:odtu-robotik", "sitemap"}
	if err := newSender(t, web).Send(t.Context(), tags); err != nil {
		t.Fatal(err)
	}
	got := web.got(t)
	if len(got) != 1 || !slices.Equal(got[0], []string{"club:odtu-robotik", "event:go-atolyesi", "sitemap"}) {
		t.Fatalf("calls = %v", got)
	}
	if _, err := webcache.NewSender(webcache.SenderConfig{BaseURL: web.srv.URL, Clock: clock.NewFake(t0)}); !errors.Is(err, webcache.ErrNotConfigured) {
		t.Fatalf("NewSender without a secret = %v", err)
	}
}

func TestSender_SplitsAbove64Tags(t *testing.T) {
	t.Parallel()
	web := newFakeWeb(t)
	var tags []string
	for i := range 130 {
		tags = append(tags, webcache.EventTag("e-"+strconv.Itoa(i)))
	}
	if err := newSender(t, web).Send(t.Context(), tags); err != nil {
		t.Fatal(err)
	}
	got := web.got(t)
	if len(got) != 3 || len(got[0]) != 64 || len(got[1]) != 64 || len(got[2]) != 2 {
		t.Fatalf("chunks = %d", len(got))
	}
}

func TestSender_RetryableAndPermanentFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status    int
		permanent bool
	}{
		{http.StatusInternalServerError, false},
		{http.StatusServiceUnavailable, false}, // koro-web without a secret yet
		{http.StatusUnauthorized, false},       // secret rotation in progress
		{http.StatusNotFound, false},           // web not deployed yet
		{http.StatusTooManyRequests, false},
		{http.StatusRequestTimeout, false},
		{http.StatusBadRequest, true},
		{http.StatusForbidden, true},
		{http.StatusRequestEntityTooLarge, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusFound, true}, // redirects are not followed
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			t.Parallel()
			web := newFakeWeb(t, c.status)
			err := newSender(t, web).Send(t.Context(), []string{"sitemap"})
			if err == nil || queue.IsPermanent(err) != c.permanent {
				t.Fatalf("Send() = %v, want permanent=%v", err, c.permanent)
			}
			if c.permanent && !errors.Is(err, webcache.ErrRejected) {
				t.Fatalf("Send() = %v, want ErrRejected", err)
			}
		})
	}
	t.Run("network", func(t *testing.T) {
		t.Parallel()
		web := newFakeWeb(t)
		web.srv.Close()
		if err := newSender(t, web).Send(t.Context(), []string{"sitemap"}); err == nil || queue.IsPermanent(err) {
			t.Fatalf("Send() to a closed server = %v, want a retryable error", err)
		}
	})
	t.Run("invalid tag", func(t *testing.T) {
		t.Parallel()
		web := newFakeWeb(t)
		if err := newSender(t, web).Send(t.Context(), []string{"Club A"}); !queue.IsPermanent(err) {
			t.Fatalf("Send(invalid) = %v, want permanent", err)
		}
		if got := web.got(t); len(got) != 0 {
			t.Fatalf("an invalid tag was sent: %v", got)
		}
	})
}

// fakeResolver serves fixed slugs, public posts and event shares.
type fakeResolver struct {
	events    map[uuid.UUID]webcache.EventRef
	orgs      map[uuid.UUID]webcache.OrgRef
	posts     map[uuid.UUID]webcache.PostRef   // the posts on the public web
	shares    map[uuid.UUID][]webcache.PostRef // by event, newest first
	err       error
	sharesErr error
	limits    *[]int // the limits EventShares was asked for
}

func (r fakeResolver) Event(_ context.Context, id uuid.UUID) (webcache.EventRef, bool, error) {
	e, ok := r.events[id]
	return e, ok, r.err
}

func (r fakeResolver) Org(_ context.Context, id uuid.UUID) (webcache.OrgRef, bool, error) {
	o, ok := r.orgs[id]
	return o, ok, r.err
}

func (r fakeResolver) Post(_ context.Context, id uuid.UUID) (webcache.PostRef, bool, error) {
	p, ok := r.posts[id]
	return p, ok, r.err
}

func (r fakeResolver) EventShares(_ context.Context, eventID uuid.UUID, limit int) ([]webcache.PostRef, error) {
	if r.limits != nil {
		*r.limits = append(*r.limits, limit)
	}
	ps := r.shares[eventID]
	return ps[:min(limit, len(ps))], r.sharesErr
}

var (
	clubID, companyID, uniID        = uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	eventID, uniEventID, sharedEvID = uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	postID, share1, share2, share3  = uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
)

func resolver() fakeResolver {
	return fakeResolver{
		events: map[uuid.UUID]webcache.EventRef{
			eventID: {Slug: "go-atolyesi", OrgID: clubID}, uniEventID: {Slug: "kariyer-gunu"}, sharedEvID: {Slug: "bahar-senligi", OrgID: clubID},
		},
		orgs: map[uuid.UUID]webcache.OrgRef{
			clubID: {Kind: "club", Slug: "odtu-robotik"}, companyID: {Kind: "company", Slug: "acme"}, uniID: {Kind: "university", Slug: "odtu"},
		},
		posts: map[uuid.UUID]webcache.PostRef{postID: {ID: postID, OrgID: clubID}},
		shares: map[uuid.UUID][]webcache.PostRef{
			sharedEvID: {{ID: share1, OrgID: clubID}, {ID: share2, OrgID: companyID}, {ID: share3, OrgID: clubID}},
		},
	}
}

// subscriptions returns the revalidator's subscriptions by name.
func subscriptions(t *testing.T, r webcache.Resolver, s *webcache.Sender) map[string]outbox.Subscription {
	t.Helper()
	reg := outbox.NewRegistry()
	webcache.New(webcache.Deps{Resolver: r, Sender: s, Log: obs.Discard()}).Subscribe(reg)
	if err := reg.Err(); err != nil {
		t.Fatal(err)
	}
	out := map[string]outbox.Subscription{}
	for _, sub := range reg.All() {
		out[sub.Name] = sub
	}
	return out
}

func envelope(t *testing.T, p domainevent.Payload) domainevent.Envelope {
	t.Helper()
	e, err := domainevent.New(p, "test", uuid.NewV7())
	if err != nil {
		t.Fatal(err)
	}
	e.ID = uuid.NewV7()
	return e
}

func TestRevalidator_TopicsToTags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, sub string
		payload   domainevent.Payload
		want      []string // nil: no request
	}{
		{"published", webcache.SubEventPublished, domainevent.EventPublished{EventID: eventID, OrgID: clubID},
			[]string{"event:go-atolyesi", "club:odtu-robotik", "sitemap"}},
		{"updated title", webcache.SubEventUpdated, domainevent.EventUpdated{EventID: eventID, OrgID: clubID, Fields: []string{"title"}},
			[]string{"event:go-atolyesi", "club:odtu-robotik"}},
		{"updated slug", webcache.SubEventUpdated,
			domainevent.EventUpdated{EventID: eventID, OrgID: clubID, Fields: []string{"slug"}, PreviousSlug: "go-atolyesi-eski"},
			[]string{"event:go-atolyesi", "event:go-atolyesi-eski", "club:odtu-robotik", "sitemap"}},
		{"updated visibility", webcache.SubEventUpdated, domainevent.EventUpdated{EventID: eventID, OrgID: companyID, Fields: []string{"visibility"}},
			[]string{"event:go-atolyesi", "company:acme", "sitemap"}},
		{"cancelled", webcache.SubEventCancelled, domainevent.EventCancelled{EventID: eventID, OrgID: clubID},
			[]string{"event:go-atolyesi", "club:odtu-robotik", "sitemap"}},
		{"missing event", webcache.SubEventCancelled, domainevent.EventCancelled{EventID: uuid.NewV7(), OrgID: clubID}, nil},
		{"university event", webcache.SubEventPublished, domainevent.EventPublished{EventID: uniEventID, OrgID: uniID},
			[]string{"event:kariyer-gunu", "sitemap"}},
		{"org profile", webcache.SubOrgUpdated, domainevent.OrgUpdated{OrgID: companyID, OrgKind: "company", Fields: []string{"description"}},
			[]string{"company:acme"}},
		{"org status", webcache.SubOrgUpdated, domainevent.OrgUpdated{OrgID: clubID, OrgKind: "club", Fields: []string{"status"}},
			[]string{"club:odtu-robotik", "sitemap"}},
		{"university profile", webcache.SubOrgUpdated, domainevent.OrgUpdated{OrgID: uniID, OrgKind: "university", Fields: []string{"name"}}, nil},
		{"missing org", webcache.SubOrgUpdated, domainevent.OrgUpdated{OrgID: uuid.NewV7(), Fields: []string{"name"}}, nil},
		{"club profile", webcache.SubClubUpdated, domainevent.ClubUpdated{ClubOrgID: clubID, Fields: []string{"join_policy"}},
			[]string{"club:odtu-robotik"}},
		{"club created", webcache.SubClubUpdated, domainevent.ClubUpdated{ClubOrgID: clubID, Fields: []string{"created"}},
			[]string{"club:odtu-robotik", "sitemap"}},
		{"sensitivity", webcache.SubClubSensitivity, domainevent.ClubSensitivityChanged{ClubOrgID: clubID, Sensitive: true},
			[]string{"club:odtu-robotik", "sitemap"}},
		{"claim approved", webcache.SubClubClaimDecided,
			domainevent.ClubClaimDecided{ClaimID: uuid.NewV7(), ClubOrgID: clubID, UserID: uuid.NewV7(), Outcome: "approved"},
			[]string{"club:odtu-robotik"}},
		{"claim rejected", webcache.SubClubClaimDecided,
			domainevent.ClubClaimDecided{ClaimID: uuid.NewV7(), ClubOrgID: clubID, UserID: uuid.NewV7(), Outcome: "rejected"}, nil},
		{"logo published", webcache.SubFilePublished,
			domainevent.FilePublished{FileID: uuid.NewV7(), Purpose: "org_logo", AttachedModule: "orgs", AttachedEntityID: companyID},
			[]string{"company:acme"}},
		{"cover published", webcache.SubFilePublished,
			domainevent.FilePublished{FileID: uuid.NewV7(), Purpose: "event_cover", AttachedModule: "events", AttachedEntityID: eventID},
			[]string{"event:go-atolyesi", "club:odtu-robotik"}},
		{"cover of a missing event", webcache.SubFilePublished,
			domainevent.FilePublished{FileID: uuid.NewV7(), Purpose: "event_cover", AttachedModule: "events", AttachedEntityID: uuid.NewV7()}, nil},
		{"published file of another owner", webcache.SubFilePublished,
			domainevent.FilePublished{FileID: uuid.NewV7(), Purpose: "org_logo", AttachedModule: "feed", AttachedEntityID: companyID}, nil},
		{"rejected logo", webcache.SubFileRejected,
			domainevent.FileRejected{FileID: uuid.NewV7(), Purpose: "org_logo", Reason: "infected", AttachedModule: "orgs", AttachedEntityID: &clubID},
			[]string{"club:odtu-robotik"}},
		{"rejected cv", webcache.SubFileRejected,
			domainevent.FileRejected{FileID: uuid.NewV7(), Purpose: "cv", Reason: "infected", AttachedModule: "students", AttachedEntityID: &clubID}, nil},
		{"rejected unattached logo", webcache.SubFileRejected,
			domainevent.FileRejected{FileID: uuid.NewV7(), Purpose: "org_logo", Reason: "unprocessable"}, nil},
		{"public club post published", webcache.SubPostChanged,
			domainevent.PostChanged{PostID: postID, OrgID: clubID, Public: true, Change: "published"},
			[]string{webcache.PostTag(postID), "club:odtu-robotik"}},
		{"public club post removed", webcache.SubPostChanged,
			domainevent.PostChanged{PostID: postID, OrgID: clubID, Public: true, Change: "removed"},
			[]string{webcache.PostTag(postID), "club:odtu-robotik"}},
		{"public university post", webcache.SubPostChanged,
			domainevent.PostChanged{PostID: postID, OrgID: uniID, Public: true, Change: "edited"},
			[]string{webcache.PostTag(postID)}},
		{"post of a missing org", webcache.SubPostChanged,
			domainevent.PostChanged{PostID: postID, OrgID: uuid.NewV7(), Public: true, Change: "hidden"},
			[]string{webcache.PostTag(postID)}},
		{"followers or commercial post", webcache.SubPostChanged,
			domainevent.PostChanged{PostID: postID, OrgID: companyID, Public: false, Change: "published"}, nil},
		// BE-150: a rejected image of a public post; the pages sharing an
		// event whose summary changes (each author org once, after the
		// event's own tags).
		{"rejected image of a public post", webcache.SubFileRejected,
			domainevent.FileRejected{FileID: uuid.NewV7(), Purpose: "post_media", Reason: "infected", AttachedModule: "feed", AttachedEntityID: &postID},
			[]string{webcache.PostTag(postID), "club:odtu-robotik"}},
		{"rejected image of a post off the public web", webcache.SubFileRejected,
			domainevent.FileRejected{FileID: uuid.NewV7(), Purpose: "post_media", Reason: "infected", AttachedModule: "feed", AttachedEntityID: &share1}, nil},
		{"shared event retitled", webcache.SubEventUpdated,
			domainevent.EventUpdated{EventID: sharedEvID, OrgID: clubID, Fields: []string{"capacity", "title"}},
			[]string{"event:bahar-senligi", "club:odtu-robotik", webcache.PostTag(share1), webcache.PostTag(share2), "company:acme", webcache.PostTag(share3)}},
		{"shared event off the public web", webcache.SubEventUpdated,
			domainevent.EventUpdated{EventID: sharedEvID, OrgID: clubID, Fields: []string{"visibility"}},
			[]string{"event:bahar-senligi", "club:odtu-robotik", "sitemap", webcache.PostTag(share1), webcache.PostTag(share2), "company:acme", webcache.PostTag(share3)}},
		{"shared event capacity", webcache.SubEventUpdated,
			domainevent.EventUpdated{EventID: sharedEvID, OrgID: clubID, Fields: []string{"capacity"}},
			[]string{"event:bahar-senligi", "club:odtu-robotik"}},
		{"shared event cancelled", webcache.SubEventCancelled, domainevent.EventCancelled{EventID: sharedEvID, OrgID: clubID},
			[]string{"event:bahar-senligi", "club:odtu-robotik", "sitemap", webcache.PostTag(share1), webcache.PostTag(share2), "company:acme", webcache.PostTag(share3)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			web := newFakeWeb(t)
			subs := subscriptions(t, resolver(), newSender(t, web))
			sub, ok := subs[c.sub]
			if !ok || sub.Topic != c.payload.Topic() || sub.Queue != queue.Default {
				t.Fatalf("subscription %s = %+v", c.sub, sub)
			}
			if err := sub.Handle(t.Context(), envelope(t, c.payload)); err != nil {
				t.Fatal(err)
			}
			got := web.got(t)
			switch {
			case c.want == nil && len(got) != 0:
				t.Fatalf("calls = %v, want none", got)
			case c.want != nil && (len(got) != 1 || !slices.Equal(got[0], c.want)):
				t.Fatalf("calls = %v, want [%v]", got, c.want)
			}
		})
	}
}

func TestRevalidator_FailuresAndDisabled(t *testing.T) {
	t.Parallel()
	pub := domainevent.EventPublished{EventID: eventID, OrgID: clubID}

	// Resolution errors retry; nothing is sent.
	web := newFakeWeb(t)
	r := resolver()
	r.err = errors.New("db down")
	err := subscriptions(t, r, newSender(t, web))[webcache.SubEventPublished].Handle(t.Context(), envelope(t, pub))
	if err == nil || queue.IsPermanent(err) || len(web.got(t)) != 0 {
		t.Fatalf("resolver failure = %v", err)
	}
	// A payload that does not decode is permanent.
	bad := envelope(t, pub)
	bad.Payload = []byte(`{"event_id":1}`)
	if err := subscriptions(t, resolver(), newSender(t, web))[webcache.SubEventPublished].Handle(t.Context(), bad); !queue.IsPermanent(err) {
		t.Fatalf("undecodable payload = %v", err)
	}
	// koro-web refuses for good: permanent (the queue archives the task).
	refusing := newFakeWeb(t, http.StatusUnprocessableEntity)
	err = subscriptions(t, resolver(), newSender(t, refusing))[webcache.SubEventPublished].Handle(t.Context(), envelope(t, pub))
	if !queue.IsPermanent(err) {
		t.Fatalf("422 = %v, want permanent", err)
	}
	// A slug outside the contract is dropped, the rest is sent.
	odd := resolver()
	odd.events[eventID] = webcache.EventRef{Slug: "Büyük Harf"}
	web2 := newFakeWeb(t)
	if err := subscriptions(t, odd, newSender(t, web2))[webcache.SubEventPublished].Handle(t.Context(), envelope(t, pub)); err != nil {
		t.Fatal(err)
	}
	if got := web2.got(t); len(got) != 1 || !slices.Equal(got[0], []string{"club:odtu-robotik", "sitemap"}) {
		t.Fatalf("calls = %v", got)
	}
	// Not configured (dev): every task is a no-op.
	for _, sub := range subscriptions(t, resolver(), nil) {
		if err := sub.Handle(t.Context(), envelope(t, pub)); err != nil {
			t.Fatalf("%s without a sender = %v", sub.Name, err)
		}
	}
}

// TestRevalidator_EventSharesCapped (BE-150): an event shared by more
// public posts than MaxEventShares revalidates the newest MaxEventShares
// pages in requests of at most MaxTags; a failing share read retries.
func TestRevalidator_EventSharesCapped(t *testing.T) {
	t.Parallel()
	r := resolver()
	var limits []int
	r.limits = &limits
	many := make([]webcache.PostRef, webcache.MaxEventShares+20)
	for i := range many {
		many[i] = webcache.PostRef{ID: uuid.NewV7(), OrgID: clubID}
	}
	r.shares = map[uuid.UUID][]webcache.PostRef{sharedEvID: many}
	web := newFakeWeb(t)
	cancelled := domainevent.EventCancelled{EventID: sharedEvID, OrgID: clubID}
	if err := subscriptions(t, r, newSender(t, web))[webcache.SubEventCancelled].Handle(t.Context(), envelope(t, cancelled)); err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, call := range web.got(t) {
		if len(call) > webcache.MaxTags {
			t.Fatalf("request of %d tags", len(call))
		}
		all = append(all, call...)
	}
	want := []string{"event:bahar-senligi", "club:odtu-robotik", "sitemap"}
	for _, p := range many[:webcache.MaxEventShares] {
		want = append(want, webcache.PostTag(p.ID))
	}
	if !slices.Equal(all, want) {
		t.Fatalf("tags = %d %v, want the event's and the newest %d post pages", len(all), all, webcache.MaxEventShares)
	}
	if len(web.got(t)) != 3 || !slices.Equal(limits, []int{webcache.MaxEventShares + 1}) {
		t.Fatalf("%d requests, limits %v", len(web.got(t)), limits)
	}

	r.sharesErr = errors.New("db down")
	web2 := newFakeWeb(t)
	err := subscriptions(t, r, newSender(t, web2))[webcache.SubEventCancelled].Handle(t.Context(), envelope(t, cancelled))
	if err == nil || queue.IsPermanent(err) || len(web2.got(t)) != 0 {
		t.Fatalf("share read failure = %v", err)
	}
}

// TestRevalidator_OutboxToTaskToHTTP: the outbox row written in the
// business transaction becomes one task per subscription; running the task
// (the worker's part) POSTs the signed tags.
func TestRevalidator_OutboxToTaskToHTTP(t *testing.T) {
	t.Parallel()
	web := newFakeWeb(t)
	clk := clock.NewFake(t0)
	subs := outbox.NewRegistry()
	webcache.New(webcache.Deps{Resolver: resolver(), Sender: newSender(t, web), Log: obs.Discard()}).Subscribe(subs)
	q := queue.NewMem()
	ob := outbox.New(outbox.Deps{Store: outbox.NewMemStore(), Tx: db.NopTx{}, Subs: subs, Queue: q, Clock: clk, Log: obs.Discard()})
	env, err := domainevent.New(domainevent.EventPublished{EventID: eventID, OrgID: clubID}, "event", eventID)
	if err != nil {
		t.Fatal(err)
	}
	if err := (db.NopTx{}).WithTx(t.Context(), func(ctx context.Context) error { return ob.Add(ctx, env) }); err != nil {
		t.Fatal(err)
	}
	if len(web.got(t)) != 0 {
		t.Fatal("the webhook ran before the relay")
	}
	if n, err := ob.NewRelay(outbox.RelayConfig{}).Once(t.Context()); err != nil || n != 1 {
		t.Fatalf("relay = %d, %v", n, err)
	}
	tasks := q.Tasks()
	if len(tasks) != 1 || tasks[0].Type != webcache.SubEventPublished || tasks[0].Queue != queue.Default {
		t.Fatalf("tasks = %+v", tasks)
	}
	byName := map[string]outbox.Subscription{}
	for _, s := range subs.All() {
		byName[s.Name] = s
	}
	if err := byName[tasks[0].Type].HandleTask(t.Context(), tasks[0].Payload); err != nil {
		t.Fatal(err)
	}
	if got := web.got(t); len(got) != 1 || !slices.Equal(got[0], []string{"event:go-atolyesi", "club:odtu-robotik", "sitemap"}) {
		t.Fatalf("calls = %v", got)
	}
}
