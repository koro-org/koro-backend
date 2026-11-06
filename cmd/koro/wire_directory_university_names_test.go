package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/directory"
	"koro/internal/identity"
	"koro/internal/platform/clock"
)

// universityReadCounter counts the directory store's university reads.
type universityReadCounter struct {
	directory.Store
	reads *atomic.Int32
}

func (s universityReadCounter) GetUniversity(ctx context.Context, id uuid.UUID) (directory.University, error) {
	s.reads.Add(1)
	return s.Store.GetUniversity(ctx, id)
}

// TestWire_DirectoryNamesEduUniversities (BE-185, T-223) through the built
// container: directory fills identity's universityNames, so the student's
// answer (getMyEduVerification, the document in review next to a verified
// row included) and the admin queue carry the name. A university directory
// does not know keeps its ID and an empty name; a queue page reads each
// university once, however many rows point at it.
func TestWire_DirectoryNamesEduUniversities(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	store := identity.NewMemStore(clock.Real().Now)
	c.identity.store = store
	reads := &atomic.Int32{}
	mem := c.directory.newStore
	c.directory.newStore = func(c *container) directory.Store {
		return universityReadCounter{Store: mem(c), reads: reads}
	}
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.identity.universityNames == nil {
		t.Fatal("directory did not bind identity's universityNames")
	}
	ctx := t.Context()
	const uniName = "Ad Çözücü Üniversitesi"
	if _, err := c.directory.svc.ImportReference(ctx, directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: "T-185", Name: uniName, City: "İzmir", Type: directory.UniversityState}},
	}); err != nil {
		t.Fatal(err)
	}
	known, err := c.directory.built.UniversityByYokCode(ctx, "T-185")
	if err != nil {
		t.Fatal(err)
	}
	unknown := uuid.NewV7()

	e := &actorEnv{c: c, store: store, clk: c.infra.clock}
	pending := func(u identity.User, uni uuid.UUID) {
		t.Helper()
		file := uuid.NewV7()
		if err := store.InsertEduVerification(ctx, identity.EduRecord{ID: uuid.NewV7(), UserID: u.ID, Method: identity.EduMethodDocument,
			UniversityID: &uni, DocumentFileID: &file, Status: identity.EduPending}); err != nil {
			t.Fatal(err)
		}
	}
	type ref struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	type edu struct {
		Status          string `json:"status"`
		University      *ref   `json:"university"`
		PendingDocument *struct {
			University *ref `json:"university"`
		} `json:"pending_document"`
	}
	do := e.http(t)
	my := func(u identity.User) edu {
		t.Helper()
		code, body := do(http.MethodGet, "/api/v1/me/edu-verification", e.session(t, u, identity.ClientMobile), "")
		if code != http.StatusOK {
			t.Fatalf("getMyEduVerification = %d %s", code, body)
		}
		var got edu
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// A document in review: its university is the one the student claimed.
	waiting := e.user(t, nil)
	pending(waiting, known.ID)
	if got := my(waiting); got.Status != "pending" || got.University == nil || got.University.Name != uniName {
		t.Fatalf("pending answer = %+v", got)
	}

	// Verified, with a renewal document in review next to it.
	renewing := e.user(t, nil)
	now := e.clk.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(24 * time.Hour)
	addr := "be185@ogr.uni.edu.tr"
	if err := store.InsertEduVerification(ctx, identity.EduRecord{ID: uuid.NewV7(), UserID: renewing.ID, Method: identity.EduMethodEmail,
		EduEmail: addr, EduEmailNormalized: addr, EmailDomain: "ogr.uni.edu.tr", UniversityID: &known.ID,
		Status: identity.EduVerified, VerifiedAt: &now, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	pending(renewing, known.ID)
	got := my(renewing)
	if got.Status != "verified" || got.University == nil || got.University.Name != uniName {
		t.Fatalf("verified answer = %+v", got)
	}
	if got.PendingDocument == nil || got.PendingDocument.University == nil || got.PendingDocument.University.Name != uniName {
		t.Fatalf("pending_document = %+v", got.PendingDocument)
	}

	// Directory does not know the university: the ID alone, not an error.
	lost := e.user(t, nil)
	pending(lost, unknown)
	if got := my(lost); got.University == nil || got.University.ID != unknown.String() || got.University.Name != "" {
		t.Fatalf("unknown university answer = %+v", got.University)
	}

	// The queue: three rows, two universities, at most two reads.
	reads.Store(0)
	admin, ok := authztest.Get("platform_admin")
	if !ok {
		t.Fatal("no platform_admin persona")
	}
	page, err := c.identity.svc.EduQueue(ctx, admin.Actor, identity.EduQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("queue = %d items, want 3", len(page.Items))
	}
	for _, it := range page.Items {
		want := uniName
		if *it.Record.UniversityID == unknown {
			want = ""
		}
		if it.UniversityName != want {
			t.Fatalf("queue row of %s named %q, want %q", *it.Record.UniversityID, it.UniversityName, want)
		}
	}
	if n := reads.Load(); n > 2 {
		t.Fatalf("university reads for a queue of 3 rows over 2 universities = %d, want at most 2", n)
	}
}
