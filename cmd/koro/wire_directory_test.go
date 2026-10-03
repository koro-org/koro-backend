package main

import (
	"context"
	"slices"
	"testing"
	"uuid"

	"koro/internal/directory"
	"koro/internal/orgs"
	"koro/internal/platform/db"
	"koro/internal/platform/queue"
)

// useMemStore installs the in-memory directory stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-020.
// Only tickets of the directory module edit this file (BE-001 R4).
// listClubs of the in-memory store reads the club orgs from the orgs
// service (the GORM store joins organizations).
func (w *directoryWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.newStore = func(c *container) directory.Store {
		return directory.NewMemStore(c.infra.clock.Now, func(ctx context.Context, id uuid.UUID) (directory.OrgRef, bool) {
			if c.orgs.svc == nil {
				return directory.OrgRef{}, false
			}
			o, err := c.orgs.svc.Get(ctx, id)
			if err != nil {
				return directory.OrgRef{}, false
			}
			return directory.OrgRef{Name: o.Name, Slug: o.Slug, Status: string(o.Status)}, true
		})
	}
}

// TestWire_DirectoryFillsHooks pins what directory binds in build (BE-001
// R4): hooks.domainResolver (identity), hooks.clubInfo (orgs joinClub) and
// orgs' universityOrgs (searchOrgs?university_id), all answering from the
// directory store; the sensitivity request task is registered.
func TestWire_DirectoryFillsHooks(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.directory.svc == nil || c.hooks.domainResolver == nil || c.hooks.clubInfo == nil || c.orgs.universityOrgs == nil {
		t.Fatal("directory did not fill its hooks")
	}
	if !slices.ContainsFunc(c.infra.jobs.Tasks(), func(task queue.Task) bool { return task.Name == directory.SensitivityRequestTaskName }) {
		t.Fatal("sensitivity request task not registered")
	}
	ctx := t.Context()
	res, err := c.directory.svc.ImportReference(ctx, directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: "T-1", Name: "Kanca Üniversitesi", City: "İzmir", Type: directory.UniversityState}},
		Domains:      []directory.RefDomain{{Domain: "kanca.edu.tr", UniversityYokCode: "T-1", DomainKind: directory.DomainMixed}},
	})
	if err != nil || res.Universities.Created != 1 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	uniID, found, err := c.hooks.domainResolver(ctx, "ogr.kanca.edu.tr")
	if err != nil || !found {
		t.Fatalf("domainResolver = %v, %v", found, err)
	}
	if _, found, _ := c.hooks.domainResolver(ctx, "example.test"); found {
		t.Fatal("domainResolver matched an unknown domain")
	}
	u, err := c.directory.svc.GetUniversity(ctx, uniID)
	if err != nil {
		t.Fatal(err)
	}
	// A club opened through the store; clubInfo reports its join facts.
	clubOrg, err := c.orgs.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Kanca Kulübü"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.directory.built.InsertClub(ctx, directory.Club{
		OrgID: clubOrg, UniversityID: uniID, Category: directory.CategoryHobby, JoinPolicy: directory.JoinOpen,
		ClaimStatus: directory.ClaimUnclaimed,
	}); err != nil {
		t.Fatal(err)
	}
	info, found, err := c.hooks.clubInfo(ctx, clubOrg)
	if err != nil || !found || info.UniversityID != uniID || info.JoinPolicy != "open" || info.Sensitive {
		t.Fatalf("clubInfo = %+v, %v, %v", info, found, err)
	}
	if _, found, err := c.hooks.clubInfo(ctx, uuid.NewV7()); err != nil || found {
		t.Fatalf("clubInfo(unknown) = %v, %v", found, err)
	}
	ids, err := c.orgs.universityOrgs(ctx, uniID)
	if err != nil || !slices.Equal(ids, []uuid.UUID{u.OrgID, clubOrg}) {
		t.Fatalf("universityOrgs = %v, %v", ids, err)
	}
}
