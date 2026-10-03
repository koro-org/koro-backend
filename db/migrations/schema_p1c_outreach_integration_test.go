//go:build integration

package migrations_test

import (
	"slices"
	"sync"
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// TestSchema_SegmentConstraints (02 §13.1): k >= 10 in the stored count,
// dimension bounds; segments are archived, not deleted.
func TestSchema_SegmentConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company, user := w.org("company"), w.user()
	ins := `INSERT INTO segments (org_id, name, definition, audience_keys, audience_dim_count, last_count, created_by) VALUES ($1, $2, '{}', ARRAY['all'], $3, $4, $5)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"a count below 10", ins, []any{company, "Küçük", 1, 9, user}, db.CodeCheckViolation, "segments_last_count_chk"},
		{"a count of 0", ins, []any{company, "Boş", 1, 0, user}, db.CodeCheckViolation, "segments_last_count_chk"},
		{"a one-letter name", ins, []any{company, "A", 1, nil, user}, db.CodeCheckViolation, "segments_name_chk"},
		{"a name of 101 characters", ins, []any{company, repeat("a", 101), 1, nil, user}, db.CodeCheckViolation, "segments_name_chk"},
		{"no dimension", ins, []any{company, "Hiç", 0, nil, user}, db.CodeCheckViolation, "segments_audience_dim_count_chk"},
		{"five dimensions", ins, []any{company, "Beş", 5, nil, user}, db.CodeCheckViolation, "segments_audience_dim_count_chk"},
		{"unknown org", ins, []any{uuid.NewV7(), "Segment", 1, nil, user}, db.CodeForeignKeyViolation, "segments_org_id_fkey"},
		{"unknown creator", ins, []any{company, "Segment", 1, nil, uuid.NewV7()}, db.CodeForeignKeyViolation, "segments_created_by_fkey"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"not counted yet (or fewer than 10)", ins, []any{company, "Yeni", 1, nil, user}, "", ""},
		{"a count of 10", ins, []any{company, "On kişi", 1, 10, user}, "", ""},
	})
	seg := w.segment(company, user)
	w.exec(`UPDATE segments SET archived_at = now() WHERE id = $1`, seg)
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw cannot delete a segment", `DELETE FROM segments WHERE id = $1`, []any{seg}, db.CodeInsufficientPriv, ""},
	})
}

// TestSchema_OutreachDispatchConstraints (02 §13.2; 06 H-38): one dispatch
// per post; the post and the segment belong to the dispatching org
// (composite FKs); channels are a non-empty subset of push/email/in_app.
func TestSchema_OutreachDispatchConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company, rival, user := w.org("company"), w.org("company"), w.user()
	post := func(org uuid.UUID) uuid.UUID {
		return w.post(org, "company", user, companySegmentPost(w.usage(org, "post")))
	}
	ownPost, rivalPost := post(company), post(rival)
	ownSeg, rivalSeg := w.segment(company, user), w.segment(rival, user)
	// $1 = org, $2 = post, $3 = segment.
	base := map[string]string{
		"org_id": "$1", "post_id": "$2", "segment_id": "$3", "audience_keys": "ARRAY['year:3']", "audience_dim_count": "1",
		"channels": "ARRAY['push','in_app']", "title_snapshot": "'Staj başvuruları açıldı'", "deep_link": "'/posts/1'",
		"commercial": "true", "created_by": lit(user.String()),
	}
	d := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("outreach_dispatches", base, over), args, state, constraint}
	}
	fresh := func() []any { return []any{company, post(company), ownSeg} }
	chk := func(name string, over map[string]string, constraint string) p1aCase {
		return d(name, over, fresh(), db.CodeCheckViolation, constraint)
	}
	expectFail(t, w.d.Pool, []p1aCase{
		chk("no channel", map[string]string{"channels": "ARRAY[]::text[]"}, "outreach_dispatches_channels_chk"),
		chk("an unknown channel", map[string]string{"channels": "ARRAY['sms']"}, "outreach_dispatches_channels_chk"),
		chk("a NULL channel", map[string]string{"channels": "ARRAY[NULL]::text[]"}, "outreach_dispatches_channels_chk"),
		chk("an absolute deep link", map[string]string{"deep_link": "'https://evil.example/x'"}, "outreach_dispatches_deep_link_chk"),
		chk("a title of 141 characters", map[string]string{"title_snapshot": lit(repeat("a", 141))}, "outreach_dispatches_title_snapshot_chk"),
		chk("unknown status", map[string]string{"status": "'paused'"}, "outreach_dispatches_status_chk"),
		chk("completed without a completion time", map[string]string{"status": "'completed'"}, "outreach_dispatches_completed_chk"),
		chk("negative eligible count", map[string]string{"eligible_count": "-1"}, "outreach_dispatches_eligible_count_chk"),
		chk("negative consented count", map[string]string{"consented_count": "-1"}, "outreach_dispatches_consented_count_chk"),
		chk("negative sent count", map[string]string{"sent_count": "-1"}, "outreach_dispatches_sent_count_chk"),
		chk("five dimensions", map[string]string{"audience_dim_count": "5"}, "outreach_dispatches_audience_dim_count_chk"),
		chk("an error of 2001 characters", map[string]string{"last_error": lit(repeat("a", 2001))}, "outreach_dispatches_last_error_chk"),
		// 06 H-38: the scope comes from the parent rows, the DB refuses a mismatch.
		d("another org's post", nil, []any{company, rivalPost, ownSeg}, db.CodeForeignKeyViolation, "outreach_dispatches_post_id_org_id_fkey"),
		d("another org's segment", nil, []any{company, post(company), rivalSeg}, db.CodeForeignKeyViolation, "outreach_dispatches_segment_id_org_id_fkey"),
		d("unknown post", nil, []any{company, uuid.NewV7(), ownSeg}, db.CodeForeignKeyViolation, "outreach_dispatches_post_id_org_id_fkey"),
		d("unknown org", nil, []any{uuid.NewV7(), post(company), nil}, db.CodeForeignKeyViolation, "outreach_dispatches_org_id_fkey"),
		d("unknown usage", map[string]string{"entitlement_usage_id": lit(uuid.NewV7().String())}, fresh(), db.CodeForeignKeyViolation, "outreach_dispatches_entitlement_usage_id_fkey"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		d("a dispatch of the own post and segment", nil, []any{company, ownPost, ownSeg}, "", ""),
		d("a dispatch without a saved segment", map[string]string{"channels": "ARRAY['email']"}, []any{company, post(company), nil}, "", ""),
		d("a completed dispatch", map[string]string{"status": "'completed'", "completed_at": "now()", "eligible_count": "40", "consented_count": "30", "sent_count": "30"}, fresh(), "", ""),
	})
	expectFail(t, w.d.Pool, []p1aCase{
		d("a second dispatch of a post", nil, []any{company, ownPost, ownSeg}, db.CodeUniqueViolation, "outreach_dispatches_post_id_uq"),
	})
	// A post with a dispatch cannot be purged before the dispatch (NO ACTION; T-146).
	expectFail(t, w.d.Pool, []p1aCase{
		{"deleting a dispatched post", `DELETE FROM posts WHERE id = $1`, []any{ownPost}, db.CodeForeignKeyViolation, "outreach_dispatches_post_id_org_id_fkey"},
	})
}

// contactDefaults are the columns of a pending discovery request;
// $1 = company, $2 = sender, $3 = student, $4 = usage.
var contactDefaults = map[string]string{
	"org_id":               "$1",
	"sender_user_id":       "$2",
	"student_user_id":      "$3",
	"via":                  "'discovery'",
	"subject":              "'Staj görüşmesi'",
	"message":              "'Profilinizi gördük, görüşmek isteriz.'",
	"expires_at":           "now() + interval '14 days'",
	"entitlement_usage_id": "$4",
}

// TestSchema_ContactRequestConstraints (02 §14.1): only a company asks;
// one pending request per company and student, also under concurrent
// requests; via ↔ post, answer ↔ time.
func TestSchema_ContactRequestConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company, club, sender, student := w.org("company"), w.org("club"), w.user(), w.student()
	usage := func() uuid.UUID { return w.usage(company, "contact_request") }
	opportunity := w.post(company, "company", sender, map[string]string{"kind": "'opportunity_call'", "commercial_flag": "true", "opportunity": `'{"role":"intern"}'::jsonb`})
	c := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("contact_requests", contactDefaults, over), args, state, constraint}
	}
	fresh := func() []any { return []any{company, sender, w.student(), usage()} }
	chk := func(name string, over map[string]string, constraint string) p1aCase {
		return c(name, over, fresh(), db.CodeCheckViolation, constraint)
	}
	opp := lit(opportunity.String())
	expectFail(t, w.d.Pool, []p1aCase{
		chk("unknown channel", map[string]string{"via": "'cold_email'"}, "contact_requests_via_chk"),
		chk("via an opportunity post without the post", map[string]string{"via": "'opportunity_post'"}, "contact_requests_opportunity_post_chk"),
		chk("via discovery with a post", map[string]string{"opportunity_post_id": opp}, "contact_requests_opportunity_post_chk"),
		chk("a subject of 2 characters", map[string]string{"subject": "'Hi'"}, "contact_requests_subject_chk"),
		chk("a message of 9 characters", map[string]string{"message": "'Merhaba!!'"}, "contact_requests_message_chk"),
		chk("a message of 1001 characters", map[string]string{"message": lit(repeat("a", 1001))}, "contact_requests_message_chk"),
		chk("unknown status", map[string]string{"status": "'read'"}, "contact_requests_status_chk"),
		chk("accepted without an answer time", map[string]string{"status": "'accepted'"}, "contact_requests_responded_chk"),
		chk("declined without an answer time", map[string]string{"status": "'declined'"}, "contact_requests_responded_chk"),
		chk("pending with an answer time", map[string]string{"responded_at": "now()"}, "contact_requests_responded_chk"),
		chk("expired with an answer time", map[string]string{"status": "'expired'", "responded_at": "now()"}, "contact_requests_responded_chk"),
		chk("a club as the requester kind", map[string]string{"org_kind": "'club'"}, "contact_requests_org_kind_chk"),
		c("a club requesting contact", nil, []any{club, sender, w.student(), usage()}, db.CodeForeignKeyViolation, "contact_requests_org_id_org_kind_fkey"),
		c("no expiry", map[string]string{"expires_at": "-"}, fresh(), codeNotNull, ""),
		c("no entitlement", map[string]string{"entitlement_usage_id": "NULL"}, []any{company, sender, w.student()}, codeNotNull, ""),
		c("unknown usage", nil, []any{company, sender, w.student(), uuid.NewV7()}, db.CodeForeignKeyViolation, "contact_requests_entitlement_usage_id_fkey"),
		c("unknown student", nil, []any{company, sender, uuid.NewV7(), usage()}, db.CodeForeignKeyViolation, "contact_requests_student_user_id_fkey"),
		c("unknown sender", nil, []any{company, uuid.NewV7(), w.student(), usage()}, db.CodeForeignKeyViolation, "contact_requests_sender_user_id_fkey"),
		c("unknown post", map[string]string{"via": "'opportunity_post'", "opportunity_post_id": lit(uuid.NewV7().String())}, fresh(), db.CodeForeignKeyViolation, "contact_requests_opportunity_post_id_fkey"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		c("via an opportunity post", map[string]string{"via": "'opportunity_post'", "opportunity_post_id": opp}, fresh(), "", ""),
		c("accepted", map[string]string{"status": "'accepted'", "responded_at": "now()"}, fresh(), "", ""),
		c("declined", map[string]string{"status": "'declined'", "responded_at": "now()"}, fresh(), "", ""),
		c("withdrawn", map[string]string{"status": "'withdrawn'"}, fresh(), "", ""),
		c("expired", map[string]string{"status": "'expired'"}, fresh(), "", ""),
	})

	// Concurrent requests of one company to one student: exactly one pending.
	var wg sync.WaitGroup
	won := make([]bool, 12)
	sql := insertSQL("contact_requests", contactDefaults, nil)
	usages := make([]uuid.UUID, len(won))
	for i := range usages {
		usages[i] = usage()
	}
	for i := range won {
		wg.Go(func() {
			_, err := w.d.Pool.Exec(t.Context(), sql, company, sender, student, usages[i])
			switch {
			case err == nil:
				won[i] = true
			case db.SQLState(err) != db.CodeUniqueViolation || db.ConstraintName(err) != "contact_requests_pending_uq":
				t.Errorf("concurrent request: %v", err)
			}
		})
	}
	wg.Wait()
	if n := len(slices.DeleteFunc(won, func(b bool) bool { return !b })); n != 1 {
		t.Errorf("concurrent pending requests accepted = %d, want 1", n)
	}
	// Once answered, a new request is possible (the 90-day rule is the service's).
	w.exec(`UPDATE contact_requests SET status = 'declined', responded_at = now() WHERE org_id = $1 AND student_user_id = $2 AND status = 'pending'`, company, student)
	expectOK(t, w.d.Pool, []p1aCase{c("a new request after the answer", nil, []any{company, sender, student, usage()}, "", "")})
	expectFail(t, w.d.Pool, []p1aCase{c("a second pending request", nil, []any{company, sender, student, usage()}, db.CodeUniqueViolation, "contact_requests_pending_uq")})
	// Another company may ask the same student.
	other := w.org("company")
	expectOK(t, w.d.Pool, []p1aCase{c("another company", nil, []any{other, sender, student, w.usage(other, "contact_request")}, "", "")})
}

// TestSchema_ContactBlockConstraints (02 §14.2).
func TestSchema_ContactBlockConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	student, company := w.student(), w.org("company")
	ins := `INSERT INTO contact_blocks (student_user_id, company_org_id) VALUES ($1, $2)`
	w.exec(ins, student, company)
	expectFail(t, w.d.Pool, []p1aCase{
		{"blocking twice", ins, []any{student, company}, db.CodeUniqueViolation, "contact_blocks_pkey"},
		{"unknown student", ins, []any{uuid.NewV7(), company}, db.CodeForeignKeyViolation, "contact_blocks_student_user_id_fkey"},
		{"unknown company", ins, []any{student, uuid.NewV7()}, db.CodeForeignKeyViolation, "contact_blocks_company_org_id_fkey"},
	})
	w.exec(`DELETE FROM contact_blocks WHERE student_user_id = $1`, student) // unblock = hard delete
}
