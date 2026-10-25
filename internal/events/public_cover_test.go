package events_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/files"
	"koro/internal/orgs"
)

// coverFiles is the files port of the cover tests: every cover is an
// event_cover of its org, cleared, signed and has a public copy.
type coverFiles struct{ org map[uuid.UUID]uuid.UUID }

func (f coverFiles) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	org, ok := f.org[id]
	if !ok {
		return files.FileMeta{}, files.ErrNotFound
	}
	return files.FileMeta{ID: id, Purpose: files.PurposeEventCover, OrgID: &org, Status: files.StatusActive, ScanStatus: files.ScanClean}, nil
}
func (coverFiles) Attach(context.Context, authz.Actor, uuid.UUID, files.Owner) error { return nil }
func (coverFiles) PresignGet(_ context.Context, id uuid.UUID, _ time.Duration) (files.Download, error) {
	return files.Download{URL: "https://s3.test/signed/" + id.String()}, nil
}
func (coverFiles) PublicURL(_ context.Context, id uuid.UUID) (string, error) {
	return publicCover(id), nil
}
func (coverFiles) PutGenerated(context.Context, files.GeneratedFile) (uuid.UUID, error) {
	return uuid.Nil(), errors.New("not in this test")
}
func (coverFiles) Delete(context.Context, authz.Actor, uuid.UUID) error { return nil }

func publicCover(id uuid.UUID) string {
	return "https://media.test/event_cover/" + id.String() + ".png"
}

// TestEvents_CoverPublicURL_OnlyPublicEvents (BE-120): the permanent cover
// URL is shown for a published public event of an active, non-sensitive
// organiser; a draft, a members-only or sensitive-category event, a
// sensitive club's event and a suspended organiser's event keep the signed
// URL only. Lists carry it too.
func TestEvents_CoverPublicURL_OnlyPublicEvents(t *testing.T) {
	t.Parallel()
	cf := coverFiles{org: map[uuid.UUID]uuid.UUID{}}
	e := newEnv(t, func(d *events.Deps) { d.Files = cf })
	withCover := func(org string, in events.EventInput) events.EventInput {
		id := uuid.NewV7()
		cf.org[id] = orgID(org)
		in.CoverFileID = &id
		return in
	}
	publish := func(actor, org string, in events.EventInput) events.EventView {
		t.Helper()
		v := e.create(t, actor, org, in)
		if err := e.mem.SetFormVersion(v.Event.ID, uuid.NewV7()); err != nil {
			t.Fatal(err)
		}
		p, err := e.svc.PublishEvent(t.Context(), persona(t, actor), v.Event.ID)
		if err != nil {
			t.Fatalf("PublishEvent: %v", err)
		}
		return p
	}
	anon := authz.Anonymous()
	get := func(id uuid.UUID, a authz.Actor) events.EventView {
		t.Helper()
		v, err := e.svc.GetEvent(t.Context(), a, id)
		if err != nil {
			t.Fatalf("GetEvent: %v", err)
		}
		return v
	}

	pub := publish("club_admin", "club_a", withCover("club_a", input("Go Atölyesi")))
	if v := get(pub.Event.ID, anon); v.CoverURL == "" || v.CoverPublicURL != publicCover(*pub.Event.CoverFileID) {
		t.Fatalf("published public event: cover_url %q, cover_public_url %q", v.CoverURL, v.CoverPublicURL)
	}
	draft := e.create(t, "club_admin", "club_a", withCover("club_a", input("Taslak")))
	if v := get(draft.Event.ID, persona(t, "club_admin")); v.CoverURL == "" || v.CoverPublicURL != "" {
		t.Fatalf("draft: cover_url %q, cover_public_url %q", v.CoverURL, v.CoverPublicURL)
	}
	members := withCover("club_a", input("Üyelere Özel"))
	members.Visibility = events.VisibilityMembers
	if v := get(publish("club_admin", "club_a", members).Event.ID, persona(t, "club_admin")); v.CoverPublicURL != "" {
		t.Fatalf("members-only event: cover_public_url %q", v.CoverPublicURL)
	}
	special := withCover("club_a", input("Hassas Kategori"))
	special.IsSensitiveCategory = true
	if v := get(publish("club_admin", "club_a", special).Event.ID, anon); v.CoverPublicURL != "" {
		t.Fatalf("sensitive-category event: cover_public_url %q", v.CoverPublicURL)
	}
	sens := publish("club_admin_sensitive", "club_sensitive", withCover("club_sensitive", input("Hassas Kulüp Buluşması")))
	if v := get(sens.Event.ID, anon); v.CoverURL == "" || v.CoverPublicURL != "" {
		t.Fatalf("sensitive club's event: cover_url %q, cover_public_url %q", v.CoverURL, v.CoverPublicURL)
	}

	page, err := e.svc.ListEvents(t.Context(), anon, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, v := range page.Items {
		want := v.Event.ID == pub.Event.ID
		if (v.CoverPublicURL != "") != want {
			t.Errorf("listEvents card %q: cover_public_url %q", v.Event.Title, v.CoverPublicURL)
		}
		if want {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("published event not listed (%d cards)", len(page.Items))
	}

	e.orgs.setStatus(orgID("club_a"), orgs.StatusSuspended)
	if v := get(pub.Event.ID, anon); v.CoverPublicURL != "" {
		t.Fatalf("suspended organiser: cover_public_url %q", v.CoverPublicURL)
	}
}
