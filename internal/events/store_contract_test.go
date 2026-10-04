package events_test

import (
	"errors"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/platform/problem"
)

// world creates the parent rows a store needs (organizations, users and
// universities: FKs in PostgreSQL; plain IDs in memory).
type world struct {
	org        func(t *testing.T) uuid.UUID
	user       func(t *testing.T) uuid.UUID
	university func(t *testing.T) uuid.UUID
	// register adds a registration to an event (BE-028's table; BE-026
	// only reads whether one exists).
	register func(t *testing.T, s events.Store, org, eventID uuid.UUID)
}

func memWorld() world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	return world{org: id, user: id, university: id, register: func(_ *testing.T, s events.Store, _, eventID uuid.UUID) {
		s.(*events.MemStore).MarkRegistered(eventID)
	}}
}

var seq atomic.Int64

// row is a valid published public event of org.
func row(org, by uuid.UUID, title string, at time.Time) events.Event {
	pub := t0
	return events.Event{
		ID: uuid.NewV7(), OrgID: org, CreatedBy: by, Title: title, Slug: "e-" + strconv.FormatInt(seq.Add(1), 10),
		Category: events.CategoryTalk, Format: events.FormatInPerson, StartsAt: at, EndsAt: at.Add(time.Hour),
		Visibility: events.VisibilityPublic, Status: events.StatusPublished, PublishedAt: &pub, RequiresRegistration: true,
		RegistrationMode: events.ModeAuto,
	}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) (events.Store, world) {
		return events.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (events.Store, world)) {
	t.Helper()
	insert := func(t *testing.T, s events.Store, e events.Event) events.Event {
		t.Helper()
		got, ok, err := s.InsertEvent(t.Context(), e)
		if err != nil || !ok {
			t.Fatalf("InsertEvent: ok=%v err=%v", ok, err)
		}
		return got
	}

	t.Run("events: insert, read, slug, update", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		e := row(org, by, "Işık ve Çağrışım", start)
		e.Summary, e.City = new("Özet"), new("İzmir")
		got := insert(t, s, e)
		if got.ID != e.ID || got.OrgID != org || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() || *got.City != "İzmir" {
			t.Fatalf("stored = %+v", got)
		}
		read, err := s.GetEvent(ctx, e.ID)
		if err != nil || read.Title != e.Title || !read.StartsAt.Equal(start) {
			t.Fatalf("GetEvent = %+v, %v", read, err)
		}
		if _, err := s.GetEvent(ctx, uuid.NewV7()); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("GetEvent(unknown) = %v", err)
		}
		// The slug is unique per org: the same slug is refused without an
		// error (the transaction stays usable), another org may take it.
		dup := row(org, by, "Aynı Kısa Ad", start)
		dup.Slug = e.Slug
		if _, ok, err := s.InsertEvent(ctx, dup); err != nil || ok {
			t.Fatalf("duplicate slug: ok=%v err=%v", ok, err)
		}
		dup.OrgID = other
		insert(t, s, dup)
		// Update: scoped to the org; updated_at moves (the ETag changes).
		next := got
		next.Title = "Yeni Başlık"
		upd, err := s.UpdateEvent(ctx, next)
		if err != nil || upd.Title != "Yeni Başlık" || !upd.UpdatedAt.After(got.UpdatedAt) && !upd.UpdatedAt.Equal(got.UpdatedAt) {
			t.Fatalf("UpdateEvent = %+v, %v", upd, err)
		}
		wrong := upd
		wrong.OrgID = other
		if _, err := s.UpdateEvent(ctx, wrong); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("update under another org = %v", err)
		}
		taken := upd
		taken.Slug = "taken-" + strconv.FormatInt(seq.Add(1), 10)
		third := row(org, by, "Üçüncü", start)
		third.Slug = taken.Slug
		insert(t, s, third)
		if _, err := s.UpdateEvent(ctx, taken); !errors.Is(err, events.ErrSlugTaken) {
			t.Fatalf("update onto a taken slug = %v", err)
		}
		// A row breaking a CHECK is 422.
		bad := row(org, by, "Ters", start)
		bad.EndsAt = bad.StartsAt
		if _, _, err := s.InsertEvent(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("CHECK violation = %v", err)
		}
	})

	t.Run("forms and staff take org_id from the event (06 H-38)", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by, staff := w.org(t), w.org(t), w.user(t), w.user(t)
		e := insert(t, s, row(org, by, "Kapsam", start))
		form := events.Form{ID: uuid.NewV7(), EventID: e.ID, DraftSchema: events.JSON(`{"schema_version":1,"fields":[]}`)}
		if err := s.InsertForm(ctx, other, form); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("form under another org = %v", err)
		}
		if err := s.InsertForm(ctx, org, form); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.HasPublishedForm(ctx, org, e.ID); err != nil || ok {
			t.Fatalf("HasPublishedForm = %v, %v", ok, err)
		}
		st := events.Staff{EventID: e.ID, UserID: staff, OrgID: other, Role: events.StaffScanner, InvitedBy: by}
		if _, err := s.InsertStaff(ctx, st); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("staff under another org = %v", err)
		}
		st.OrgID = org
		got, err := s.InsertStaff(ctx, st)
		if err != nil || got.OrgID != org || got.CreatedAt.IsZero() {
			t.Fatalf("InsertStaff = %+v, %v", got, err)
		}
		if _, err := s.InsertStaff(ctx, st); !errors.Is(err, events.ErrStaffExists) {
			t.Fatalf("staff twice = %v", err)
		}
		if r, ok, err := s.StaffRole(ctx, org, e.ID, staff); err != nil || !ok || r != events.StaffScanner {
			t.Fatalf("StaffRole = %v, %v, %v", r, ok, err)
		}
		if _, ok, _ := s.StaffRole(ctx, other, e.ID, staff); ok {
			t.Fatal("StaffRole under another org")
		}
		if _, err := s.UpdateStaffRole(ctx, other, e.ID, staff, events.StaffManager); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("UpdateStaffRole under another org = %v", err)
		}
		if u, err := s.UpdateStaffRole(ctx, org, e.ID, staff, events.StaffManager); err != nil || u.Role != events.StaffManager {
			t.Fatalf("UpdateStaffRole = %+v, %v", u, err)
		}
		if l, err := s.ListStaff(ctx, org, e.ID); err != nil || len(l) != 1 {
			t.Fatalf("ListStaff = %+v, %v", l, err)
		}
		if l, err := s.ListStaff(ctx, other, e.ID); err != nil || len(l) != 0 {
			t.Fatalf("ListStaff under another org = %+v, %v", l, err)
		}
		if err := s.DeleteStaff(ctx, other, e.ID, staff); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("DeleteStaff under another org = %v", err)
		}
		if mine, err := s.StaffOfUser(ctx, staff); err != nil || len(mine) != 1 || mine[0].EventID != e.ID {
			t.Fatalf("StaffOfUser = %+v, %v", mine, err)
		}
		if err := s.DeleteStaff(ctx, org, e.ID, staff); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteStaff(ctx, org, e.ID, staff); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("DeleteStaff twice = %v", err)
		}
		st.Role = events.StaffScanner
		if _, err := s.InsertStaff(ctx, st); err != nil {
			t.Fatal(err)
		}
		if n, err := s.DeleteStaffOfUser(ctx, staff); err != nil || n != 1 {
			t.Fatalf("DeleteStaffOfUser = %d, %v", n, err)
		}
	})

	t.Run("only a draft is deleted, with its form and staff", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by, staff := w.org(t), w.org(t), w.user(t), w.user(t)
		d := row(org, by, "Taslak", start)
		d.Status, d.PublishedAt = events.StatusDraft, nil
		d = insert(t, s, d)
		if err := s.InsertForm(ctx, org, events.Form{ID: uuid.NewV7(), EventID: d.ID, DraftSchema: events.JSON(`{"schema_version":1,"fields":[]}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertStaff(ctx, events.Staff{EventID: d.ID, UserID: staff, OrgID: org, Role: events.StaffScanner, InvitedBy: by}); err != nil {
			t.Fatal(err)
		}
		p := insert(t, s, row(org, by, "Yayımlı", start))
		if err := s.DeleteDraft(ctx, org, p.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("DeleteDraft(published) = %v", err)
		}
		if err := s.DeleteDraft(ctx, other, d.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("DeleteDraft under another org = %v", err)
		}
		if err := s.DeleteDraft(ctx, org, d.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetEvent(ctx, d.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("deleted draft = %v", err)
		}
		if rows, _ := s.StaffOfUser(ctx, staff); len(rows) != 0 {
			t.Fatalf("staff left: %+v", rows)
		}
		if _, err := s.GetEvent(ctx, p.ID); err != nil {
			t.Fatalf("published event: %v", err)
		}
	})

	t.Run("list: visibility, search, filters, keyset", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, club, by, staff := w.org(t), w.org(t), w.user(t), w.user(t)
		uni := w.university(t)
		pub := row(org, by, "Yapay Zekâ Söyleşisi", start)
		pub.City = new("İstanbul")
		pub.Summary = new("Derin öğrenme üzerine")
		pub = insert(t, s, pub)
		uniEv := row(org, by, "Kampüs Buluşması", start.Add(time.Hour))
		uniEv.Visibility, uniEv.VisibilityUniversityID = events.VisibilityUniversity, &uni
		uniEv = insert(t, s, uniEv)
		memEv := row(club, by, "Üye Toplantısı", start.Add(2*time.Hour))
		memEv.Visibility = events.VisibilityMembers
		memEv = insert(t, s, memEv)
		draft := row(org, by, "Taslak Söyleşi", start)
		draft.Status, draft.PublishedAt = events.StatusDraft, nil
		insert(t, s, draft)
		if _, err := s.InsertStaff(ctx, events.Staff{EventID: memEv.ID, UserID: staff, OrgID: club, Role: events.StaffScanner, InvitedBy: by}); err != nil {
			t.Fatal(err)
		}
		ids := func(f events.ListFilter, after *events.After, limit int) []uuid.UUID {
			t.Helper()
			rows, err := s.ListEvents(ctx, f, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			out := make([]uuid.UUID, len(rows))
			for i, r := range rows {
				out[i] = r.ID
			}
			return out
		}
		all := []uuid.UUID{pub.ID, uniEv.ID, memEv.ID}
		for _, tc := range []struct {
			name string
			v    events.Viewer
			want []uuid.UUID
		}{
			{"anonymous", events.Viewer{}, []uuid.UUID{pub.ID}},
			{"student of the university", events.Viewer{UniversityID: &uni}, []uuid.UUID{pub.ID, uniEv.ID}},
			{"member of the club", events.Viewer{MemberOrgs: []uuid.UUID{club}}, []uuid.UUID{pub.ID, memEv.ID}},
			{"staff of the members event", events.Viewer{UserID: &staff}, []uuid.UUID{pub.ID, memEv.ID}},
			{"platform", events.Viewer{All: true}, all},
		} {
			if got := ids(events.ListFilter{Viewer: tc.v}, nil, 10); !slices.Equal(got, tc.want) {
				t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
			}
		}
		everyone := events.Viewer{All: true}
		for _, tc := range []struct {
			name string
			f    events.ListFilter
			want []uuid.UUID
		}{
			{"full text, Turkish folding", events.ListFilter{Viewer: everyone, Q: "YAPAY ZEKA"}, []uuid.UUID{pub.ID}},
			{"part of the title", events.ListFilter{Viewer: everyone, Q: "buluş"}, []uuid.UUID{uniEv.ID}},
			{"summary", events.ListFilter{Viewer: everyone, Q: "öğrenme"}, []uuid.UUID{pub.ID}},
			{"a LIKE wildcard is literal", events.ListFilter{Viewer: everyone, Q: "%_"}, []uuid.UUID{}},
			{"city", events.ListFilter{Viewer: everyone, City: "ISTANBUL"}, []uuid.UUID{pub.ID}},
			{"orgs", events.ListFilter{Viewer: everyone, OrgIDs: []uuid.UUID{club}}, []uuid.UUID{memEv.ID}},
			{"no org", events.ListFilter{Viewer: everyone, OrgIDs: []uuid.UUID{}}, []uuid.UUID{}},
			{"category", events.ListFilter{Viewer: everyone, Category: events.CategoryWorkshop}, []uuid.UUID{}},
			{"starts range", events.ListFilter{Viewer: everyone, StartsFrom: new(start.Add(time.Hour)), StartsTo: new(start.Add(2 * time.Hour))}, []uuid.UUID{uniEv.ID}},
			{"ended", events.ListFilter{Viewer: everyone, EndsAfter: new(start.Add(150 * time.Minute))}, []uuid.UUID{memEv.ID}},
		} {
			if got := ids(tc.f, nil, 10); !slices.Equal(got, tc.want) {
				t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
			}
		}
		// Keyset pages by starts_at, then by publication (newest first).
		first := ids(events.ListFilter{Viewer: everyone}, nil, 2)
		if !slices.Equal(first, all[:2]) {
			t.Fatalf("first page = %v", first)
		}
		if rest := ids(events.ListFilter{Viewer: everyone}, &events.After{At: uniEv.StartsAt, ID: uniEv.ID}, 2); !slices.Equal(rest, all[2:]) {
			t.Fatalf("second page = %v", rest)
		}
		if got := ids(events.ListFilter{Viewer: everyone, Sort: events.SortPublishedNew}, nil, 10); len(got) != 3 {
			t.Fatalf("by publication = %v", got)
		}
		// The org list: drafts included, status filter.
		orgRows, err := s.ListOrgEvents(ctx, org, events.OrgListFilter{}, nil, 10)
		if err != nil || len(orgRows) != 3 {
			t.Fatalf("ListOrgEvents = %d, %v", len(orgRows), err)
		}
		drafts, err := s.ListOrgEvents(ctx, org, events.OrgListFilter{Statuses: []events.Status{events.StatusDraft}}, nil, 10)
		if err != nil || len(drafts) != 1 || drafts[0].Status != events.StatusDraft {
			t.Fatalf("drafts = %+v, %v", drafts, err)
		}
	})
}
