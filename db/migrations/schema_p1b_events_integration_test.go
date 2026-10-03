//go:build integration

package migrations_test

import (
	"sync"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_EventConstraints (02 §11.1).
func TestSchema_EventConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, creator := w.org("club"), w.user()
	uni, _ := w.university()
	// $1 = org, $2 = creator, $3 = event id.
	ev := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("events", eventDefaults, over), args, state, constraint}
	}
	ok := func() []any { return []any{org, creator, uuid.NewV7()} }
	chk := func(name string, over map[string]string, constraint string) p1aCase {
		return ev(name, over, ok(), db.CodeCheckViolation, constraint)
	}
	draft := map[string]string{"status": lit("draft"), "published_at": "-"}
	unknown := lit(uuid.NewV7().String())
	expectFail(t, w.d.Pool, []p1aCase{
		chk("title of 2 characters", map[string]string{"title": lit("ab")}, "events_title_chk"),
		chk("title of 161 characters", map[string]string{"title": lit(repeat("a", 161))}, "events_title_chk"),
		chk("upper-case slug", map[string]string{"slug": lit("Bad-Slug")}, "events_slug_chk"),
		chk("underscore in the slug", map[string]string{"slug": lit("bad_slug")}, "events_slug_chk"),
		chk("slug with a trailing dash", map[string]string{"slug": lit("slug-")}, "events_slug_chk"),
		chk("slug with a double dash", map[string]string{"slug": lit("a--b")}, "events_slug_chk"),
		chk("slug of 101 characters", map[string]string{"slug": lit(repeat("a", 101))}, "events_slug_chk"),
		chk("summary of 301 characters", map[string]string{"summary": lit(repeat("a", 301))}, "events_summary_chk"),
		chk("description of 20001 characters", map[string]string{"description_text": lit(repeat("a", 20001))}, "events_description_text_chk"),
		chk("unknown category", map[string]string{"category": lit("party")}, "events_category_chk"),
		chk("unknown format", map[string]string{"format": lit("telepathy")}, "events_format_chk"),
		chk("location of 201 characters", map[string]string{"location_name": lit(repeat("a", 201))}, "events_location_name_chk"),
		chk("address of 501 characters", map[string]string{"address": lit(repeat("a", 501))}, "events_address_chk"),
		chk("city of 61 characters", map[string]string{"city": lit(repeat("a", 61))}, "events_city_chk"),
		// 02 §0.4: the online link is https only; a protocol-relative or script URL is not a link.
		chk("http online URL", map[string]string{"online_url": lit("http://meet.example.test/x")}, "events_online_url_chk"),
		chk("protocol-relative online URL", map[string]string{"online_url": lit("//evil.example/x")}, "events_online_url_chk"),
		chk("script online URL", map[string]string{"online_url": lit("javascript:alert(1)")}, "events_online_url_chk"),
		chk("online URL without the second slash", map[string]string{"online_url": lit("https:/x")}, "events_online_url_chk"),
		chk("online URL of 2049 characters", map[string]string{"online_url": lit("https://" + repeat("a", 2041))}, "events_online_url_chk"),
		chk("an online event without a link", map[string]string{"format": lit("online")}, "events_online_url_required_chk"),
		chk("a hybrid event without a link", map[string]string{"format": lit("hybrid")}, "events_online_url_required_chk"),
		chk("unknown visibility", map[string]string{"visibility": lit("friends")}, "events_visibility_chk"),
		chk("university visibility without a university", map[string]string{"visibility": lit("university")}, "events_visibility_university_chk"),
		chk("a university on a public event", map[string]string{"visibility_university_id": lit(uni.String())}, "events_visibility_university_chk"),
		chk("unknown status", map[string]string{"status": lit("archived")}, "events_status_chk"),
		chk("published without a publication time", map[string]string{"published_at": "-"}, "events_published_chk"),
		chk("completed without a publication time", map[string]string{"status": lit("completed"), "published_at": "-"}, "events_published_chk"),
		chk("cancelled without a cancellation time", map[string]string{"status": lit("cancelled")}, "events_cancelled_chk"),
		chk("a cancellation time on a published event", map[string]string{"cancelled_at": "now()"}, "events_cancelled_chk"),
		chk("cancel reason of 501 characters", map[string]string{"status": lit("cancelled"), "cancelled_at": "now()", "cancel_reason": lit(repeat("a", 501))}, "events_cancel_reason_chk"),
		chk("unknown registration mode", map[string]string{"registration_mode": lit("lottery")}, "events_registration_mode_chk"),
		chk("capacity of zero", map[string]string{"capacity": "0"}, "events_capacity_chk"),
		chk("negative confirmed count", map[string]string{"confirmed_count": "-1"}, "events_confirmed_count_chk"),
		chk("an event that ends when it starts", map[string]string{"ends_at": "now() + interval '1 day'"}, "events_period_chk"),
		chk("an event that ends before it starts", map[string]string{"ends_at": "now()"}, "events_period_chk"),
		chk("registration closing before it opens", map[string]string{"registration_opens_at": "now() + interval '20 hours'", "registration_closes_at": "now() + interval '10 hours'"}, "events_registration_window_chk"),
		chk("registration closing after the event ends", map[string]string{"registration_closes_at": "now() + interval '3 days'"}, "events_registration_close_chk"),
		chk("capacity without registration", map[string]string{"requires_registration": "false", "capacity": "10"}, "events_registration_required_chk"),
		chk("check-in without registration", map[string]string{"requires_registration": "false", "checkin_enabled": "true"}, "events_registration_required_chk"),
		chk("a waiting list without a capacity", map[string]string{"waitlist_enabled": "true"}, "events_waitlist_capacity_chk"),
		chk("attendance closed without check-in", map[string]string{"attendance_finalized_at": "now()"}, "events_attendance_chk"),
		ev("unknown org", nil, []any{uuid.NewV7(), creator, uuid.NewV7()}, db.CodeForeignKeyViolation, "events_org_id_fkey"),
		ev("unknown creator", nil, []any{org, uuid.NewV7(), uuid.NewV7()}, db.CodeForeignKeyViolation, "events_created_by_fkey"),
		ev("unknown cover file", map[string]string{"cover_file_id": unknown}, ok(), db.CodeForeignKeyViolation, "events_cover_file_id_fkey"),
		ev("unknown university", map[string]string{"visibility": lit("university"), "visibility_university_id": unknown}, ok(), db.CodeForeignKeyViolation, "events_visibility_university_id_fkey"),
		ev("unknown finaliser", map[string]string{"checkin_enabled": "true", "attendance_finalized_at": "now()", "attendance_finalized_by": unknown}, ok(), db.CodeForeignKeyViolation, "events_attendance_finalized_by_fkey"),
	})

	expectOK(t, w.d.Pool, []p1aCase{
		ev("a draft online event without a link", cols(draft, map[string]string{"format": lit("online")}), ok(), "", ""),
		ev("a draft with only the required columns", draft, ok(), "", ""),
		ev("an online event", map[string]string{"format": lit("online"), "online_url": lit("https://meet.example.test/room")}, ok(), "", ""),
		ev("a university event", map[string]string{"visibility": lit("university"), "visibility_university_id": lit(uni.String())}, ok(), "", ""),
		ev("a cancelled event", map[string]string{"status": lit("cancelled"), "cancelled_at": "now()", "cancel_reason": lit("Hava koşulları")}, ok(), "", ""),
		ev("a completed event", map[string]string{"status": lit("completed")}, ok(), "", ""),
		ev("an event with capacity and a waiting list", map[string]string{"capacity": "50", "waitlist_enabled": "true", "checkin_enabled": "true",
			"registration_opens_at": "now()", "registration_closes_at": "now() + interval '1 day'"}, ok(), "", ""),
		ev("an event without registration", map[string]string{"requires_registration": "false"}, ok(), "", ""),
		ev("attendance closed by the system", map[string]string{"checkin_enabled": "true", "attendance_finalized_at": "now()"}, ok(), "", ""),
		ev("a title with Turkish letters", map[string]string{"title": lit("Işık ve Çağrışım Atölyesi")}, ok(), "", ""),
	})

	// (org, slug) is unique; the same slug under another org is another event.
	dup := map[string]string{"slug": lit("go-atolyesi")}
	expectOK(t, w.d.Pool, []p1aCase{ev("first event with the slug", dup, ok(), "", "")})
	expectFail(t, w.d.Pool, []p1aCase{ev("the slug twice in one org", dup, ok(), db.CodeUniqueViolation, "events_org_slug_uq")})
	expectOK(t, w.d.Pool, []p1aCase{ev("the slug in another org", dup, []any{w.org("club"), creator, uuid.NewV7()}, "", "")})
}

// TestSchema_EventCapacityCheck (02 §11.1): the capacity cannot be exceeded.
// The service keeps it with a conditional UPDATE; the CHECK is the second
// layer that holds even for a careless statement.
func TestSchema_EventCapacityCheck(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, creator := w.org("club"), w.user()

	// An event cannot be created over its capacity, nor with a negative count.
	expectFail(t, w.d.Pool, []p1aCase{
		{"created over capacity", insertSQL("events", eventDefaults, map[string]string{"capacity": "5", "confirmed_count": "6"}), []any{org, creator, uuid.NewV7()}, db.CodeCheckViolation, "events_capacity_fill_chk"},
		{"created with a negative count", insertSQL("events", eventDefaults, map[string]string{"capacity": "5", "confirmed_count": "-1"}), []any{org, creator, uuid.NewV7()}, db.CodeCheckViolation, "events_confirmed_count_chk"},
		{"created with capacity zero", insertSQL("events", eventDefaults, map[string]string{"capacity": "0"}), []any{org, creator, uuid.NewV7()}, db.CodeCheckViolation, "events_capacity_chk"},
	})
	full := w.event(org, creator, map[string]string{"capacity": "5", "confirmed_count": "5"}) // exactly full is fine
	unlimited := w.event(org, creator, nil)
	w.exec(`UPDATE events SET confirmed_count = 100000 WHERE id = $1`, unlimited) // no capacity, no limit

	expectFail(t, w.d.Pool, []p1aCase{
		{"a careless increment past capacity", `UPDATE events SET confirmed_count = confirmed_count + 1 WHERE id = $1`, []any{full}, db.CodeCheckViolation, "events_capacity_fill_chk"},
		{"capacity cut below the confirmed count", `UPDATE events SET capacity = 4 WHERE id = $1`, []any{full}, db.CodeCheckViolation, "events_capacity_fill_chk"},
		{"decrement below zero", `UPDATE events SET confirmed_count = confirmed_count - 6 WHERE id = $1`, []any{full}, db.CodeCheckViolation, "events_confirmed_count_chk"},
	})
	w.exec(`UPDATE events SET capacity = 6 WHERE id = $1`, full) // raising it is fine
	w.exec(`UPDATE events SET capacity = NULL WHERE id = $1`, full)

	// The conditional UPDATE of the service (02 §11.1): sequential seats.
	const take = `UPDATE events SET confirmed_count = confirmed_count + 1
		WHERE id = $1 AND (capacity IS NULL OR confirmed_count < capacity)`
	three := w.event(org, creator, map[string]string{"capacity": "3"})
	for i := range 3 {
		tag, err := w.d.Pool.Exec(t.Context(), take, three)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("seat %d: %v, %d rows", i+1, err, tag.RowsAffected())
		}
	}
	if tag, err := w.d.Pool.Exec(t.Context(), take, three); err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("the fourth seat: %v, %d rows; want 0 (event full)", err, tag.RowsAffected())
	}

	// … and under concurrency: 25 callers, 7 seats.
	race := w.event(org, creator, map[string]string{"capacity": "7"})
	var wg sync.WaitGroup
	got := make([]int64, 25)
	for i := range got {
		wg.Go(func() {
			tag, err := w.d.Pool.Exec(t.Context(), take, race)
			if err != nil {
				t.Errorf("concurrent seat: %v", err)
				return
			}
			got[i] = tag.RowsAffected()
		})
	}
	wg.Wait()
	var seats int64
	for _, n := range got {
		seats += n
	}
	if seats != 7 {
		t.Errorf("concurrent seats granted = %d, want 7", seats)
	}
	if n := w.count(`SELECT confirmed_count FROM events WHERE id = $1`, race); n != 7 {
		t.Errorf("confirmed_count = %d, want 7", n)
	}
}

// TestSchema_EventDeleteOnlyDraft (02 §11.1 "taslak hard delete serbest"): a
// draft can be deleted; every other event stays (a published event leaves
// registrations, tickets and an immutable form behind).
func TestSchema_EventDeleteOnlyDraft(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, creator := w.org("club"), w.user()
	owner := testutil.OwnerConn(t, w.d)

	draft := w.event(org, creator, map[string]string{"status": lit("draft"), "published_at": "-"})
	w.exec(`DELETE FROM events WHERE id = $1`, draft)
	if n := w.count(`SELECT count(*) FROM events WHERE id = $1`, draft); n != 0 {
		t.Errorf("the draft was not deleted")
	}

	published := w.event(org, creator, nil)
	cancelled := w.event(org, creator, map[string]string{"status": lit("cancelled"), "cancelled_at": "now()"})
	completed := w.event(org, creator, map[string]string{"status": lit("completed")})
	for name, id := range map[string]uuid.UUID{"published": published, "cancelled": cancelled, "completed": completed} {
		expectFail(t, w.d.Pool, []p1aCase{{"app_rw deletes a " + name + " event", `DELETE FROM events WHERE id = $1`, []any{id}, db.CodeRestrictViolation, ""}})
		expectFail(t, owner, []p1aCase{{"the owner deletes a " + name + " event", `DELETE FROM events WHERE id = $1`, []any{id}, db.CodeRestrictViolation, ""}})
	}
	// A draft that was published is no longer a draft.
	again := w.event(org, creator, map[string]string{"status": lit("draft"), "published_at": "-"})
	w.exec(`UPDATE events SET status = 'published', published_at = now() WHERE id = $1`, again)
	expectFail(t, w.d.Pool, []p1aCase{{"delete after publishing", `DELETE FROM events WHERE id = $1`, []any{again}, db.CodeRestrictViolation, ""}})
	if n := w.count(`SELECT count(*) FROM events WHERE org_id = $1`, org); n != 4 {
		t.Errorf("events left = %d, want 4", n)
	}
	// A draft with an unpublished form can go once the form is deleted first.
	d2 := w.event(org, creator, map[string]string{"status": lit("draft"), "published_at": "-"})
	w.form(d2, org)
	expectFail(t, w.d.Pool, []p1aCase{{"a draft that still has a form", `DELETE FROM events WHERE id = $1`, []any{d2}, db.CodeForeignKeyViolation, ""}})
	w.exec(`DELETE FROM event_forms WHERE event_id = $1`, d2)
	w.exec(`DELETE FROM events WHERE id = $1`, d2)
}

// TestSchema_EventSearchVector (02 §0.3, §11.1): the generated search_vector
// follows the Turkish text-search configuration and the title has a
// koro_norm trigram index, so a search without accents finds the event.
func TestSchema_EventSearchVector(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, creator := w.org("club"), w.user()
	id := w.event(org, creator, map[string]string{"title": lit("İstanbul Yazılım Atölyesi"), "summary": lit("Go ile web servisleri"), "description_text": lit("Uygulamalı çalıştay")})
	for _, tc := range []struct{ query, want string }{
		{"istanbul", "true"}, {"İSTANBUL", "true"}, {"atölyesi", "true"}, {"web", "true"}, {"çalıştay", "true"}, {"ankara", "false"},
	} {
		got := queryString(t, w.d, `SELECT (search_vector @@ websearch_to_tsquery('koro_tr', $2))::text FROM events WHERE id = $1`, id, tc.query)
		if got != tc.want {
			t.Errorf("search_vector @@ %q = %s, want %s", tc.query, got, tc.want)
		}
	}
	if got := queryString(t, w.d, `SELECT (koro_norm(title) % koro_norm($2))::text FROM events WHERE id = $1`, id, "Istanbul Yazilim Atolyesi"); got != "true" {
		t.Errorf("accent-free trigram match = %s, want true", got)
	}
}

// TestSchema_EventStaffAndFormConstraints: event_staff (02 §11.2) and
// event_forms (02 §11.3).
func TestSchema_EventStaffAndFormConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, creator, scanner := w.org("club"), w.user(), w.user()
	ev := w.event(org, creator, nil)
	other := uuid.NewV7()
	st := func(name, role string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, `INSERT INTO event_staff (event_id, user_id, org_id, role, invited_by) VALUES ($1, $2, $3, ` + lit(role) + `, $4)`, args, state, constraint}
	}
	expectFail(t, w.d.Pool, []p1aCase{
		st("unknown role", "owner", []any{ev, scanner, org, creator}, db.CodeCheckViolation, "event_staff_role_chk"),
		st("unknown event", "scanner", []any{other, scanner, org, creator}, db.CodeForeignKeyViolation, "event_staff_event_id_fkey"),
		st("unknown user", "scanner", []any{ev, other, org, creator}, db.CodeForeignKeyViolation, "event_staff_user_id_fkey"),
		st("unknown org", "scanner", []any{ev, scanner, other, creator}, db.CodeForeignKeyViolation, "event_staff_org_id_fkey"),
		st("unknown inviter", "scanner", []any{ev, scanner, org, other}, db.CodeForeignKeyViolation, "event_staff_invited_by_fkey"),
		st("no inviter", "scanner", []any{ev, scanner, org, nil}, codeNotNull, ""),
	})
	expectOK(t, w.d.Pool, []p1aCase{st("scanner", "scanner", []any{ev, scanner, org, creator}, "", "")})
	expectFail(t, w.d.Pool, []p1aCase{st("the same person twice", "manager", []any{ev, scanner, org, creator}, db.CodeUniqueViolation, "event_staff_pkey")})
	w.exec(`DELETE FROM event_staff WHERE event_id = $1`, ev) // hard delete after the event

	// event_forms: one form per event, an empty draft by default.
	f := func(name string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, `INSERT INTO event_forms (event_id, org_id) VALUES ($1, $2)`, args, state, constraint}
	}
	expectFail(t, w.d.Pool, []p1aCase{
		f("form of an unknown event", []any{other, org}, db.CodeForeignKeyViolation, "event_forms_event_id_fkey"),
		f("form of an unknown org", []any{ev, other}, db.CodeForeignKeyViolation, "event_forms_org_id_fkey"),
	})
	form := w.form(ev, org)
	expectFail(t, w.d.Pool, []p1aCase{
		f("a second form for the event", []any{ev, org}, db.CodeUniqueViolation, "event_forms_event_id_uq"),
		{"current version of nowhere", `UPDATE event_forms SET current_version_id = $2 WHERE id = $1`, []any{form, other}, db.CodeForeignKeyViolation, "event_forms_current_version_id_fkey"},
		{"unknown editor", `UPDATE event_forms SET updated_by = $2 WHERE id = $1`, []any{form, other}, db.CodeForeignKeyViolation, "event_forms_updated_by_fkey"},
	})
	if got := queryString(t, w.d, `SELECT draft_schema::text FROM event_forms WHERE id = $1`, form); got != `{"fields": [], "schema_version": 1}` {
		t.Errorf("default draft schema = %s", got)
	}
}

// TestSchema_EventIDOrgUnique (06 H-55, T-229): events (id, org_id) is
// unique, so the composite FK of a child row (offerings, order lines;
// BE-034) pins the event's org. Mutation test: a child table with
// FOREIGN KEY (event_id, org_id) REFERENCES events (id, org_id) takes the
// event under its own org and refuses it under another org; once the
// constraint is dropped, such an FK cannot even be declared.
func TestSchema_EventIDOrgUnique(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, other, creator := w.org("club"), w.org("club"), w.user()
	ev := uuid.NewV7()
	w.exec(insertSQL("events", eventDefaults, nil), org, creator, ev)
	if got := queryString(t, w.d, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'events_id_org_uq' AND conrelid = 'events'::regclass`); got != "UNIQUE (id, org_id)" {
		t.Fatalf("events_id_org_uq = %q", got)
	}
	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), `CREATE TABLE event_child (id int PRIMARY KEY, event_id uuid NOT NULL, org_id uuid NOT NULL,
		CONSTRAINT event_child_event_org_fkey FOREIGN KEY (event_id, org_id) REFERENCES events (id, org_id))`); err != nil {
		t.Fatal(err)
	}
	if got := execState(t, owner, `INSERT INTO event_child VALUES (1, $1, $2)`, ev, org); got != "" {
		t.Fatalf("child under the event's org: SQLSTATE %q", got)
	}
	if got := execState(t, owner, `INSERT INTO event_child VALUES (2, $1, $2)`, ev, other); got != db.CodeForeignKeyViolation {
		t.Fatalf("child under another org: SQLSTATE %q, want %s", got, db.CodeForeignKeyViolation)
	}
	// Mutation: without the constraint the composite FK has no target.
	if _, err := owner.Exec(t.Context(), `DROP TABLE event_child; ALTER TABLE events DROP CONSTRAINT events_id_org_uq`); err != nil {
		t.Fatal(err)
	}
	if got := execState(t, owner, `CREATE TABLE event_child2 (event_id uuid, org_id uuid,
		FOREIGN KEY (event_id, org_id) REFERENCES events (id, org_id))`); got != "42830" { // invalid_foreign_key
		t.Fatalf("composite FK without events_id_org_uq: SQLSTATE %q, want 42830", got)
	}
}
