//go:build integration

package migrations_test

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// TestSchema_EventRegistrationConstraints (02 §11.6): each status carries its
// timestamps, purged answers are really empty, and a user holds at most one
// live registration per event.
func TestSchema_EventRegistrationConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, creator := w.org("club"), w.user()
	ev := w.event(org, creator, nil)
	form := w.form(ev, org)
	version := w.formVersion(form, ev, creator, nil)
	// $1 = event, $2 = org, $3 = user, $4 = registration id.
	reg := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("event_registrations", registrationDefaults, over), args, state, constraint}
	}
	fresh := func() []any { return []any{ev, org, w.student(), uuid.NewV7()} }
	chk := func(name string, over map[string]string, constraint string) p1aCase {
		return reg(name, over, fresh(), db.CodeCheckViolation, constraint)
	}
	by := map[string]string{"cancelled_by_kind": lit("user")}
	unknown := lit(uuid.NewV7().String())
	expectFail(t, w.d.Pool, []p1aCase{
		chk("unknown status", map[string]string{"status": lit("approved")}, "event_registrations_status_chk"),
		chk("unknown source", map[string]string{"source": lit("kiosk")}, "event_registrations_source_chk"),
		chk("waitlisted without a waiting time", map[string]string{"status": lit("waitlisted"), "confirmed_at": "-"}, "event_registrations_waitlisted_chk"),
		chk("confirmed without a confirmation time", map[string]string{"confirmed_at": "-"}, "event_registrations_confirmed_chk"),
		chk("checked in without a confirmation time", map[string]string{"status": lit("checked_in"), "checked_in_at": "now()", "confirmed_at": "-"}, "event_registrations_confirmed_chk"),
		chk("no-show without a confirmation time", map[string]string{"status": lit("no_show"), "confirmed_at": "-"}, "event_registrations_confirmed_chk"),
		chk("cancelled without a cancellation time", map[string]string{"status": lit("cancelled"), "confirmed_at": "-", "cancelled_by_kind": lit("user")}, "event_registrations_cancelled_chk"),
		chk("cancelled without a canceller", map[string]string{"status": lit("cancelled"), "confirmed_at": "-", "cancelled_at": "now()"}, "event_registrations_cancelled_chk"),
		chk("withdrawn without a cancellation time", map[string]string{"status": lit("withdrawn"), "confirmed_at": "-", "cancelled_by_kind": lit("user")}, "event_registrations_cancelled_chk"),
		chk("checked in without a check-in time", map[string]string{"status": lit("checked_in")}, "event_registrations_checked_in_chk"),
		chk("sensitive answers without a purge time", map[string]string{"answers_sensitive_enc": "'\\x0102'::bytea"}, "event_registrations_sensitive_purge_chk"),
		chk("purged answers that are not empty", map[string]string{"answers": `'{"k":"v"}'::jsonb`, "answers_purged_at": "now()"}, "event_registrations_purged_chk"),
		chk("purged answers with sensitive data left", map[string]string{"answers_sensitive_enc": "'\\x0102'::bytea", "sensitive_purge_at": "now()", "answers_purged_at": "now()"}, "event_registrations_purged_chk"),
		chk("decision note of 501 characters", map[string]string{"decision_note": lit(repeat("a", 501))}, "event_registrations_decision_note_chk"),
		chk("unknown canceller kind", map[string]string{"cancelled_by_kind": lit("admin")}, "event_registrations_cancelled_by_kind_chk"),
		chk("cancel reason of 301 characters", map[string]string{"cancel_reason": lit(repeat("a", 301))}, "event_registrations_cancel_reason_chk"),
	})
	expectFail(t, w.d.Pool, []p1aCase{
		reg("no purge time for the answers", map[string]string{"answers_purge_at": "-"}, fresh(), codeNotNull, ""),
		reg("unknown event", nil, []any{uuid.NewV7(), org, w.student(), uuid.NewV7()}, db.CodeForeignKeyViolation, "event_registrations_event_id_fkey"),
		reg("unknown org", nil, []any{ev, uuid.NewV7(), w.student(), uuid.NewV7()}, db.CodeForeignKeyViolation, "event_registrations_org_id_fkey"),
		reg("unknown user", nil, []any{ev, org, uuid.NewV7(), uuid.NewV7()}, db.CodeForeignKeyViolation, "event_registrations_user_id_fkey"),
		reg("unknown form version", map[string]string{"form_version_id": unknown}, fresh(), db.CodeForeignKeyViolation, "event_registrations_form_version_id_fkey"),
		reg("unknown approver", map[string]string{"approved_by": unknown}, fresh(), db.CodeForeignKeyViolation, "event_registrations_approved_by_fkey"),
	})

	// Every status with the timestamps it needs.
	statuses := map[string]map[string]string{
		"pending":    {"status": lit("pending"), "confirmed_at": "-"},
		"waitlisted": {"status": lit("waitlisted"), "confirmed_at": "-", "waitlisted_at": "now()"},
		"rejected":   {"status": lit("rejected"), "confirmed_at": "-", "approved_by": lit(creator.String()), "approved_at": "now()"},
		"expired":    {"status": lit("expired"), "confirmed_at": "-", "waitlisted_at": "now()"},
		"confirmed":  {"form_version_id": lit(version.String())},
		"checked_in": {"status": lit("checked_in"), "checked_in_at": "now()"},
		"no_show":    {"status": lit("no_show")},
		"cancelled":  cols(by, map[string]string{"status": lit("cancelled"), "cancelled_at": "now()"}),
		"withdrawn":  cols(by, map[string]string{"status": lit("withdrawn"), "confirmed_at": "-", "cancelled_at": "now()"}),
	}
	for name, over := range statuses {
		expectOK(t, w.d.Pool, []p1aCase{reg("a "+name+" registration", over, fresh(), "", "")})
	}
	// Answers purged by the schedule: the columns are emptied, the row stays.
	user := w.student()
	id := w.registration(ev, org, user, map[string]string{"answers": `'{"k":"v"}'::jsonb`, "answers_sensitive_enc": "'\\x0102'::bytea", "sensitive_purge_at": "now()"})
	w.exec(`UPDATE event_registrations SET answers = '{}', answers_sensitive_enc = NULL, answers_purged_at = now() WHERE id = $1`, id)

	// One live registration per (event, user): pending, confirmed, waitlisted,
	// checked-in and no-show block a new one; cancelled, rejected, expired and
	// withdrawn do not.
	for _, live := range []string{"pending", "confirmed", "waitlisted", "checked_in", "no_show"} {
		u := w.student()
		args := []any{ev, org, u, uuid.NewV7()}
		expectOK(t, w.d.Pool, []p1aCase{reg("first "+live, statuses[live], args, "", "")})
		expectFail(t, w.d.Pool, []p1aCase{reg("a second registration next to a "+live+" one", nil, []any{ev, org, u, uuid.NewV7()}, db.CodeUniqueViolation, "event_registrations_active_uq")})
	}
	for _, over := range []string{"cancelled", "rejected", "expired", "withdrawn"} {
		u := w.student()
		expectOK(t, w.d.Pool, []p1aCase{
			reg("first, then "+over, statuses[over], []any{ev, org, u, uuid.NewV7()}, "", ""),
			reg("a new registration after a "+over+" one", nil, []any{ev, org, u, uuid.NewV7()}, "", ""),
		})
	}
	// The same user may register for another event.
	other := w.event(org, creator, nil)
	expectOK(t, w.d.Pool, []p1aCase{reg("another event", nil, []any{other, org, user, uuid.NewV7()}, "", "")})
}

// TestSchema_RegistrationTicketConstraints (02 §11.7): token digest and manual
// code shape, one active ticket per registration, and the revoked-state CHECK
// (H-20 fix: an active ticket cannot carry half of a revocation).
func TestSchema_RegistrationTicketConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, creator := w.org("club"), w.user()
	ev := w.event(org, creator, nil)
	student := w.student()
	reg := w.registration(ev, org, student, nil)
	// $1 = registration, $2 = event, $3 = ticket id, $4 = token digest, $5 = manual code.
	tk := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("registration_tickets", ticketDefaults, over), args, state, constraint}
	}
	args := func(r uuid.UUID) []any { return []any{r, ev, uuid.NewV7(), bytes32("t-" + w.n()), w.manualCode()} }
	withArg := func(a []any, i int, v any) []any { a[i] = v; return a }
	revoked := map[string]string{"status": lit("revoked"), "revoked_at": "now()", "revoke_reason": lit("admin")}
	expectFail(t, w.d.Pool, []p1aCase{
		tk("version 0", map[string]string{"version": "0"}, args(reg), db.CodeCheckViolation, "registration_tickets_version_chk"),
		tk("a token digest of the wrong length", nil, withArg(args(reg), 3, []byte{1, 2, 3}), db.CodeCheckViolation, "registration_tickets_token_lookup_chk"),
		tk("a manual code of 7 characters", nil, withArg(args(reg), 4, "0123456"), db.CodeCheckViolation, "registration_tickets_manual_code_chk"),
		tk("a manual code of 9 characters", nil, withArg(args(reg), 4, "012345678"), db.CodeCheckViolation, "registration_tickets_manual_code_chk"),
		tk("a lower-case manual code", nil, withArg(args(reg), 4, "abcdefgh"), db.CodeCheckViolation, "registration_tickets_manual_code_chk"),
		tk("a manual code with an I (Crockford)", nil, withArg(args(reg), 4, "ABCDEFGI"), db.CodeCheckViolation, "registration_tickets_manual_code_chk"),
		tk("a manual code with an L", nil, withArg(args(reg), 4, "ABCDEFGL"), db.CodeCheckViolation, "registration_tickets_manual_code_chk"),
		tk("a manual code with an O", nil, withArg(args(reg), 4, "ABCDEFGO"), db.CodeCheckViolation, "registration_tickets_manual_code_chk"),
		tk("a manual code with a U", nil, withArg(args(reg), 4, "ABCDEFGU"), db.CodeCheckViolation, "registration_tickets_manual_code_chk"),
		tk("unknown status", map[string]string{"status": lit("used")}, args(reg), db.CodeCheckViolation, "registration_tickets_status_chk"),
		tk("unknown revoke reason", map[string]string{"status": lit("revoked"), "revoked_at": "now()", "revoke_reason": lit("lost")}, args(reg), db.CodeCheckViolation, "registration_tickets_revoke_reason_chk"),
		// The revoked state is all or nothing.
		tk("revoked without a time and a reason", map[string]string{"status": lit("revoked")}, args(reg), db.CodeCheckViolation, "registration_tickets_revoked_chk"),
		tk("revoked without a reason", map[string]string{"status": lit("revoked"), "revoked_at": "now()"}, args(reg), db.CodeCheckViolation, "registration_tickets_revoked_chk"),
		tk("revoked without a time", map[string]string{"status": lit("revoked"), "revoke_reason": lit("admin")}, args(reg), db.CodeCheckViolation, "registration_tickets_revoked_chk"),
		tk("an active ticket with a revocation time only", map[string]string{"revoked_at": "now()"}, args(reg), db.CodeCheckViolation, "registration_tickets_revoked_chk"),
		tk("an active ticket with a revoke reason only", map[string]string{"revoke_reason": lit("admin")}, args(reg), db.CodeCheckViolation, "registration_tickets_revoked_chk"),
		tk("an active ticket with a full revocation", map[string]string{"revoked_at": "now()", "revoke_reason": lit("admin")}, args(reg), db.CodeCheckViolation, "registration_tickets_revoked_chk"),
		tk("unknown registration", nil, args(uuid.NewV7()), db.CodeForeignKeyViolation, "registration_tickets_registration_id_fkey"),
		tk("unknown event", nil, withArg(args(reg), 1, uuid.NewV7()), db.CodeForeignKeyViolation, "registration_tickets_event_id_fkey"),
	})

	first := args(reg)
	expectOK(t, w.d.Pool, []p1aCase{tk("the first ticket", nil, first, "", "")})
	expectFail(t, w.d.Pool, []p1aCase{
		tk("a second active ticket", map[string]string{"version": "2"}, args(reg), db.CodeUniqueViolation, "registration_tickets_active_uq"),
		tk("the same token digest", map[string]string{"version": "2", "status": lit("revoked"), "revoked_at": "now()", "revoke_reason": lit("reissued")}, withArg(args(reg), 3, first[3]), db.CodeUniqueViolation, "registration_tickets_token_lookup_uq"),
		tk("the same version twice", revoked, args(reg), db.CodeUniqueViolation, "registration_tickets_registration_version_uq"),
		tk("the same manual code at the event", map[string]string{"version": "2", "status": lit("revoked"), "revoked_at": "now()", "revoke_reason": lit("reissued")}, withArg(args(reg), 4, first[4]), db.CodeUniqueViolation, "registration_tickets_event_manual_code_uq"),
	})
	// Re-issue: revoke the old ticket, then version 2 is the only active one.
	w.exec(`UPDATE registration_tickets SET status = 'revoked', revoked_at = now(), revoke_reason = 'reissued' WHERE registration_id = $1`, reg)
	expectOK(t, w.d.Pool, []p1aCase{tk("the re-issued ticket", map[string]string{"version": "2"}, args(reg), "", "")})
	expectFail(t, w.d.Pool, []p1aCase{
		{"revoking half", `UPDATE registration_tickets SET revoke_reason = 'admin' WHERE registration_id = $1 AND status = 'active'`, []any{reg}, db.CodeCheckViolation, "registration_tickets_revoked_chk"},
		{"un-revoking by status only", `UPDATE registration_tickets SET status = 'active' WHERE registration_id = $1 AND version = 1`, []any{reg}, db.CodeCheckViolation, "registration_tickets_revoked_chk"},
	})
	// The same manual code at another event is another code.
	other := w.event(org, creator, nil)
	reg2 := w.registration(other, org, student, nil)
	expectOK(t, w.d.Pool, []p1aCase{tk("the same manual code at another event", nil, []any{reg2, other, uuid.NewV7(), bytes32("other"), first[4]}, "", "")})

	// Tickets (and check-ins) go with the registration (ON DELETE CASCADE).
	ticket := w.ticket(reg2, other, map[string]string{"version": "2", "status": lit("revoked"), "revoked_at": "now()", "revoke_reason": lit("reissued")})
	w.exec(insertSQL("event_checkins", checkinDefaults, nil), checkinArgs(other, org, reg2, ticket, creator, uuid.NewV7())...)
	w.exec(`DELETE FROM event_registrations WHERE id = $1`, reg2)
	if n := w.count(`SELECT (SELECT count(*) FROM registration_tickets WHERE registration_id = $1) + (SELECT count(*) FROM event_checkins WHERE registration_id = $1)`, reg2); n != 0 {
		t.Errorf("tickets and check-ins left after deleting the registration = %d", n)
	}
}

// TestSchema_CheckinPartialUnique (02 §11.8): a registration has at most one
// valid check-in; a second one is refused even under concurrent scans, and a
// voided check-in frees the slot.
func TestSchema_CheckinPartialUnique(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, creator, staff := w.org("club"), w.user(), w.user()
	ev := w.event(org, creator, nil)
	reg := w.registration(ev, org, w.student(), nil)
	ticket := w.ticket(reg, ev, nil)
	// $1 = event, $2 = org, $3 = registration, $4 = ticket, $5 = staff, $6 = check-in id.
	in := func(name string, over map[string]string, r, tk uuid.UUID, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("event_checkins", checkinDefaults, over), checkinArgs(ev, org, r, tk, staff, uuid.NewV7()), state, constraint}
	}
	first := uuid.NewV7()
	w.exec(insertSQL("event_checkins", checkinDefaults, nil), checkinArgs(ev, org, reg, ticket, staff, first)...)

	// A second valid check-in for the same registration is refused: another
	// scan, the manual code, another device, even the same ticket again.
	expectFail(t, w.d.Pool, []p1aCase{
		in("a second scan", nil, reg, ticket, db.CodeUniqueViolation, "event_checkins_one_active_uq"),
		in("a second check-in by manual code", map[string]string{"method": lit("manual_code"), "device_id": lit("kapi-2")}, reg, ticket, db.CodeUniqueViolation, "event_checkins_one_active_uq"),
	})
	// A different registration is unaffected.
	reg2 := w.registration(ev, org, w.student(), nil)
	ticket2 := w.ticket(reg2, ev, nil)
	expectOK(t, w.d.Pool, []p1aCase{in("another registration", nil, reg2, ticket2, "", "")})

	// Voiding keeps the row and frees the slot; then exactly one new check-in fits.
	expectFail(t, w.d.Pool, []p1aCase{
		{"a void without the voider", `UPDATE event_checkins SET voided_at = now(), void_reason = 'Yanlış kişi' WHERE id = $1`, []any{first}, db.CodeCheckViolation, "event_checkins_void_actor_chk"},
		{"a void without the reason", `UPDATE event_checkins SET voided_at = now(), voided_by = $2 WHERE id = $1`, []any{first, staff}, db.CodeCheckViolation, "event_checkins_void_text_chk"},
		{"a voider without a void", `UPDATE event_checkins SET voided_by = $2 WHERE id = $1`, []any{first, staff}, db.CodeCheckViolation, "event_checkins_void_actor_chk"},
		{"a void reason of 4 characters", `UPDATE event_checkins SET voided_at = now(), voided_by = $2, void_reason = 'hata' WHERE id = $1`, []any{first, staff}, db.CodeCheckViolation, "event_checkins_void_reason_chk"},
		{"a void reason of 301 characters", `UPDATE event_checkins SET voided_at = now(), voided_by = $2, void_reason = $3 WHERE id = $1`, []any{first, staff, repeat("a", 301)}, db.CodeCheckViolation, "event_checkins_void_reason_chk"},
		{"an unknown voider", `UPDATE event_checkins SET voided_at = now(), voided_by = $2, void_reason = 'Yanlış kişi' WHERE id = $1`, []any{first, uuid.NewV7()}, db.CodeForeignKeyViolation, "event_checkins_voided_by_fkey"},
	})
	w.exec(`UPDATE event_checkins SET voided_at = now(), voided_by = $2, void_reason = 'Yanlış kişi' WHERE id = $1`, first, staff)
	if n := w.count(`SELECT count(*) FROM event_checkins WHERE registration_id = $1`, reg); n != 1 {
		t.Fatalf("a voided check-in must stay: rows = %d", n)
	}
	expectOK(t, w.d.Pool, []p1aCase{in("the check-in after a void", nil, reg, ticket, "", "")})
	expectFail(t, w.d.Pool, []p1aCase{in("a second check-in after the void", nil, reg, ticket, db.CodeUniqueViolation, "event_checkins_one_active_uq")})
	// Voided rows pile up freely.
	second := w.count(`SELECT count(*) FROM event_checkins WHERE registration_id = $1 AND voided_at IS NULL`, reg)
	if second != 1 {
		t.Fatalf("valid check-ins = %d, want 1", second)
	}
	w.exec(`UPDATE event_checkins SET voided_at = now(), voided_by = $2, void_reason = 'Tekrar iptal' WHERE registration_id = $1 AND voided_at IS NULL`, reg, staff)
	if n := w.count(`SELECT count(*) FROM event_checkins WHERE registration_id = $1 AND voided_at IS NOT NULL`, reg); n != 2 {
		t.Errorf("voided check-ins = %d, want 2", n)
	}

	// Concurrent scans of one ticket: exactly one wins.
	reg3 := w.registration(ev, org, w.student(), nil)
	ticket3 := w.ticket(reg3, ev, nil)
	var wg sync.WaitGroup
	won := make([]bool, 12)
	for i := range won {
		wg.Go(func() {
			_, err := w.d.Pool.Exec(t.Context(), insertSQL("event_checkins", checkinDefaults, nil), checkinArgs(ev, org, reg3, ticket3, staff, uuid.NewV7())...)
			switch {
			case err == nil:
				won[i] = true
			case db.SQLState(err) != db.CodeUniqueViolation || db.ConstraintName(err) != "event_checkins_one_active_uq":
				t.Errorf("concurrent scan: %v", err)
			}
		})
	}
	wg.Wait()
	wins := 0
	for _, ok := range won {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("concurrent scans accepted = %d, want 1", wins)
	}

	// The remaining shape rules.
	scan := uuid.NewV7()
	w.exec(insertSQL("event_checkins", checkinDefaults, map[string]string{"client_scan_id": "$7"}), append(checkinArgs(ev, org, w.registration(ev, org, w.student(), nil), ticket2, staff, uuid.NewV7()), scan)...)
	dup := insertSQL("event_checkins", checkinDefaults, map[string]string{"client_scan_id": "$7"})
	expectFail(t, w.d.Pool, []p1aCase{
		{"a repeated client scan id", dup, append(checkinArgs(ev, org, w.registration(ev, org, w.student(), nil), ticket2, staff, uuid.NewV7()), scan), db.CodeUniqueViolation, "event_checkins_client_scan_id_uq"},
		in("unknown method", map[string]string{"method": lit("nfc")}, w.registration(ev, org, w.student(), nil), ticket2, db.CodeCheckViolation, "event_checkins_method_chk"),
		in("a device id of 101 characters", map[string]string{"device_id": lit(repeat("a", 101))}, w.registration(ev, org, w.student(), nil), ticket2, db.CodeCheckViolation, "event_checkins_device_id_chk"),
		in("unknown registration", nil, uuid.NewV7(), ticket2, db.CodeForeignKeyViolation, "event_checkins_registration_id_fkey"),
		in("unknown ticket", nil, w.registration(ev, org, w.student(), nil), uuid.NewV7(), db.CodeForeignKeyViolation, "event_checkins_ticket_id_fkey"),
	})
}

// TestSchema_EventDataExportConstraints (02 §11.9).
func TestSchema_EventDataExportConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, user := w.org("club"), w.user()
	ev := w.event(org, user, nil)
	file := w.file("event_export")
	// $1 = event, $2 = org, $3 = requester, $4 = file.
	base := map[string]string{
		"event_id": "$1", "org_id": "$2", "requested_by": "$3",
		"reason": lit("Katılımcı listesi sponsor toplantısı için"), "fields": "ARRAY['first_name','last_name']",
		"expires_at": "now() + interval '7 days'",
	}
	x := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("event_data_exports", base, over), args, state, constraint}
	}
	ok := []any{ev, org, user, file}
	fieldList := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = lit("f" + strconv.Itoa(i))
		}
		return "ARRAY[" + strings.Join(parts, ",") + "]"
	}
	ready := map[string]string{"status": lit("ready"), "file_id": "$4", "row_count": "120", "file_sha256": "decode(repeat('ab', 32), 'hex')", "ip": lit("203.0.113.7")}
	expectFail(t, w.d.Pool, []p1aCase{
		x("a reason of 9 characters", map[string]string{"reason": lit("123456789")}, ok[:3], db.CodeCheckViolation, "event_data_exports_reason_chk"),
		x("a reason of 501 characters", map[string]string{"reason": lit(repeat("a", 501))}, ok[:3], db.CodeCheckViolation, "event_data_exports_reason_chk"),
		x("no fields", map[string]string{"fields": "ARRAY[]::text[]"}, ok[:3], db.CodeCheckViolation, "event_data_exports_fields_chk"),
		x("41 fields", map[string]string{"fields": fieldList(41)}, ok[:3], db.CodeCheckViolation, "event_data_exports_fields_chk"),
		x("unknown status", map[string]string{"status": lit("done")}, ok[:3], db.CodeCheckViolation, "event_data_exports_status_chk"),
		x("negative row count", map[string]string{"row_count": "-1"}, ok[:3], db.CodeCheckViolation, "event_data_exports_row_count_chk"),
		x("a digest of the wrong length", map[string]string{"file_sha256": "'\\x0102'::bytea"}, ok[:3], db.CodeCheckViolation, "event_data_exports_file_sha256_chk"),
		x("negative download count", map[string]string{"download_count": "-1"}, ok[:3], db.CodeCheckViolation, "event_data_exports_download_count_chk"),
		x("ready without a file", map[string]string{"status": lit("ready")}, ok[:3], db.CodeCheckViolation, "event_data_exports_ready_chk"),
		x("purged without a purge time", map[string]string{"status": lit("purged")}, ok[:3], db.CodeCheckViolation, "event_data_exports_purged_chk"),
		x("a purge time on a queued export", map[string]string{"purged_at": "now()"}, ok[:3], db.CodeCheckViolation, "event_data_exports_purged_chk"),
		x("purged but the file is still linked", map[string]string{"status": lit("purged"), "purged_at": "now()", "file_id": "$4"}, ok, db.CodeCheckViolation, "event_data_exports_purged_file_chk"),
		x("no expiry", map[string]string{"expires_at": "-"}, ok[:3], codeNotNull, ""),
		x("unknown event", nil, []any{uuid.NewV7(), org, user}, db.CodeForeignKeyViolation, "event_data_exports_event_id_fkey"),
		x("unknown org", nil, []any{ev, uuid.NewV7(), user}, db.CodeForeignKeyViolation, "event_data_exports_org_id_fkey"),
		x("unknown requester", nil, []any{ev, org, uuid.NewV7()}, db.CodeForeignKeyViolation, "event_data_exports_requested_by_fkey"),
		x("unknown file", map[string]string{"status": lit("ready"), "file_id": "$4"}, []any{ev, org, user, uuid.NewV7()}, db.CodeForeignKeyViolation, "event_data_exports_file_id_fkey"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		x("a queued export", nil, ok[:3], "", ""),
		x("a ready export", ready, ok, "", ""),
		x("a purged export", map[string]string{"status": lit("purged"), "purged_at": "now()", "row_count": "120"}, ok[:3], "", ""),
		x("a failed export", map[string]string{"status": lit("failed")}, ok[:3], "", ""),
		x("40 fields", map[string]string{"fields": fieldList(40)}, ok[:3], "", ""),
	})
	if got := queryString(t, w.d, `SELECT include_sensitive::text || download_count::text FROM event_data_exports ORDER BY created_at LIMIT 1`); got != "false0" {
		t.Errorf("export defaults = %q, want include_sensitive false and 0 downloads", got)
	}
}
