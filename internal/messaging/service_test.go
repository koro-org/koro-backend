package messaging_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/entitlements"
	"koro/internal/feed"
	"koro/internal/messaging"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/students"
)

// TestCreateContactRequest_IdempotencyReplay_SingleConsume (04 §1.8, 06
// H-5): the same Idempotency-Key and body replays the first answer without a
// second unit, row, event or audit record; the same key with another body
// is 422 idempotency.key_reused; without a key a second request to the same
// student is 409 (one pending request per pair).
func TestCreateContactRequest_IdempotencyReplay_SingleConsume(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cmm := actor(t, "company_member")
	ctx := withKey(t.Context(), cmm, "create-key-0001")
	in := request(e.companyA, "student")
	first, err := e.svc.Create(ctx, cmm, e.companyA, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.Create(ctx, cmm, e.companyA, in)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again.Request.ID != first.Request.ID {
		t.Fatalf("replay answered request %s, want %s", again.Request.ID, first.Request.ID)
	}
	if n := len(e.ent.consumed()); n != 1 || e.ent.left(e.companyA) != 4 {
		t.Fatalf("units consumed %d (left %d), want exactly one", n, e.ent.left(e.companyA))
	}
	if c := e.ent.consumed()[0]; c.ref != (entitlements.Ref{Type: entitlements.RefContactRequest, ID: first.Request.ID}) ||
		c.usage != first.Request.EntitlementUsageID {
		t.Fatalf("consumption %+v does not match the request %+v", c, first.Request)
	}
	if n := len(e.outbox.of(domainevent.TopicContactRequestCreated)); n != 1 {
		t.Fatalf("created events = %d, want 1", n)
	}
	if n := len(e.audit.of(authz.MessagingContactRequestCreate)); n != 1 {
		t.Fatalf("audit records = %d, want 1", n)
	}
	// Unit, row, event and audit share the request's transaction (01 §6.3).
	tx := e.ent.consumed()[0].tx
	if e.outbox.of(domainevent.TopicContactRequestCreated)[0].tx != tx || e.audit.of(authz.MessagingContactRequestCreate)[0].tx != tx {
		t.Fatal("unit, event and audit record are not in one transaction")
	}

	other := in
	other.Subject = "Başka bir konu"
	_, err = e.svc.Create(ctx, cmm, e.companyA, other)
	problemtest.WantErr(t, err, idempotency.ErrKeyReused)

	_, err = e.svc.Create(t.Context(), cmm, e.companyA, in)
	problemtest.WantProblem(t, err, 409, "messaging.request_pending")
	if n := len(e.ent.consumed()); n != 1 {
		t.Fatalf("a refused request consumed a unit (%d)", n)
	}

	if first.Status != messaging.StatusPending || !first.Request.ExpiresAt.Equal(t0.Add(messaging.RequestTTL)) ||
		first.AnonID != in.AnonID || first.Request.Via != messaging.ViaDiscovery {
		t.Fatalf("request = %+v", first)
	}
}

// TestCreateContactRequest_NoQuota_403NotEntitled: a company without a
// contact_request unit gets 403 entitlements.not_entitled and nothing is
// written; the 404 of an unqualified target comes first (k19 order).
func TestCreateContactRequest_NoQuota_403NotEntitled(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.ent.units[e.companyA] = 0
	cmm := actor(t, "company_member")
	_, err := e.svc.Create(t.Context(), cmm, e.companyA, request(e.companyA, "student"))
	problemtest.WantErr(t, err, entitlements.ErrNoEntitlement)
	problemtest.WantProblem(t, err, 403, "entitlements.not_entitled")
	if rows, _ := e.store.ReceivedBy(t.Context(), authztest.ID("student")); len(rows) != 0 {
		t.Fatalf("rows written: %+v", rows)
	}
	if len(e.outbox.of(domainevent.TopicContactRequestCreated)) != 0 || len(e.audit.of(authz.MessagingContactRequestCreate)) != 0 {
		t.Fatal("event or audit record without a unit")
	}
	_, err = e.svc.Create(t.Context(), cmm, e.companyA, request(e.companyA, "student_minor"))
	problemtest.WantErr(t, err, problem.ErrNotFound)
}

// TestCreateContactRequest_MinorTarget_404 (03 §2.3, k19): a minor is
// never discoverable; their pseudonym is the same 404 as an unknown or
// malformed one, another company's pseudonym, a student who left level 2.
func TestCreateContactRequest_MinorTarget_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cmm := actor(t, "company_member")
	ctx := t.Context()
	cases := map[string]messaging.CreateInput{
		"minor":            request(e.companyA, "student_minor"),
		"unknown":          {AnonID: "zzzzzzzzzzzzzzzz", Subject: "Yaz stajı", Message: "Görüşmek isteriz, uygun musunuz?"},
		"other company":    request(e.companyB, "student"),
		"not discoverable": request(e.companyA, "student_b"),
	}
	for name, in := range cases {
		_, err := e.svc.Create(ctx, cmm, e.companyA, in)
		if !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("%s: %v, want 404", name, err)
		}
	}
	// Discoverable once, then level 2 off: the pseudonym the company holds is 404.
	e.students.set(authztest.ID("student_granted"), false)
	_, err := e.svc.Create(ctx, cmm, e.companyA, request(e.companyA, "student_granted"))
	problemtest.WantErr(t, err, problem.ErrNotFound)
	if len(e.ent.consumed()) != 0 {
		t.Fatal("a refused target consumed a unit")
	}
}

// TestCreateContactRequest_BlockedByStudent_404: a student who blocked the
// company is the same 404 for it (02 §14.2: the company never learns of the
// block); another company still reaches the student; unblocking opens it
// again.
func TestCreateContactRequest_BlockedByStudent_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	stu := actor(t, "student")
	if err := e.svc.Block(ctx, stu, e.companyA); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Create(ctx, actor(t, "company_member"), e.companyA, request(e.companyA, "student"))
	problemtest.WantErr(t, err, problem.ErrNotFound)
	if _, err := e.svc.Create(ctx, actor(t, "other_company_member"), e.companyB, request(e.companyB, "student")); err != nil {
		t.Fatalf("company_b: %v", err)
	}
	if err := e.svc.Unblock(ctx, stu, e.companyA); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, actor(t, "company_member"), e.companyA, request(e.companyA, "student")); err != nil {
		t.Fatalf("after unblock: %v", err)
	}
}

// TestCreate_DeclinedWithin90Days_404 (03 k19): after a decline the
// company may not ask the student again for 90 days; an expired or
// withdrawn request does not count.
func TestCreate_DeclinedWithin90Days_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cmm := actor(t, "company_member")
	v := e.send(t, "student")
	if _, err := e.svc.Respond(ctx, actor(t, "student"), v.Request.ID, decline); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(89 * 24 * time.Hour)
	_, err := e.svc.Create(ctx, cmm, e.companyA, request(e.companyA, "student"))
	problemtest.WantErr(t, err, problem.ErrNotFound)
	e.clk.Advance(24 * time.Hour)
	if _, err := e.svc.Create(ctx, cmm, e.companyA, request(e.companyA, "student")); err != nil {
		t.Fatalf("after 90 days: %v", err)
	}
	// A withdrawn request does not hold the next one back.
	w := e.send(t, "student_granted")
	if _, err := e.svc.Withdraw(ctx, cmm, e.companyA, w.Request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, cmm, e.companyA, request(e.companyA, "student_granted")); err != nil {
		t.Fatalf("after a withdrawal: %v", err)
	}
}

// TestCreate_CompanyStatusAndValidation (03 §2.3, 04 §2.9): a suspended
// company may not send (403 orgs.org_suspended), one in review neither (403
// orgs.org_not_verified); a club is not a company (404); the body is 422
// before anything is read; an opportunity call must be a published one of
// the company.
func TestCreate_CompanyStatusAndValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cmm := actor(t, "company_member")
	in := request(e.companyA, "student")

	e.orgs.status(e.companyA, orgs.StatusSuspended)
	_, err := e.svc.Create(ctx, cmm, e.companyA, in)
	problemtest.WantErr(t, err, orgs.ErrOrgSuspended)
	e.orgs.status(e.companyA, orgs.StatusPendingReview)
	_, err = e.svc.Create(ctx, cmm, e.companyA, in)
	problemtest.WantErr(t, err, orgs.ErrOrgNotVerified)
	e.orgs.status(e.companyA, orgs.StatusActive)

	// Authorisation first: staff 403, outsiders 404 (03 §7.6).
	_, err = e.svc.Create(ctx, actor(t, "platform_admin"), e.companyA, in)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.Create(ctx, actor(t, "student"), e.companyA, in)
	problemtest.WantErr(t, err, authz.ErrNotFound)

	for name, bad := range map[string]messaging.CreateInput{
		"short subject": {AnonID: in.AnonID, Subject: "Hi", Message: in.Message},
		"short message": {AnonID: in.AnonID, Subject: in.Subject, Message: "Merhaba!!"},
		"long message":  {AnonID: in.AnonID, Subject: in.Subject, Message: strings.Repeat("a", 1001)},
		"no anon_id":    {Subject: in.Subject, Message: in.Message},
		"bidi subject":  {AnonID: in.AnonID, Subject: "Yaz\u202estajı", Message: in.Message},
	} {
		_, err := e.svc.Create(ctx, cmm, e.companyA, bad)
		if !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("%s: %v, want 422", name, err)
		}
	}

	pub, draft, other, text := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	e.posts.posts[pub] = feed.PostSummary{ID: pub, OrgID: e.companyA, Kind: feed.KindOpportunityCall, Status: feed.StatusPublished, Title: "Staj ilanı"}
	e.posts.posts[draft] = feed.PostSummary{ID: draft, OrgID: e.companyA, Kind: feed.KindOpportunityCall, Status: feed.StatusDraft}
	e.posts.posts[other] = feed.PostSummary{ID: other, OrgID: e.companyB, Kind: feed.KindOpportunityCall, Status: feed.StatusPublished}
	e.posts.posts[text] = feed.PostSummary{ID: text, OrgID: e.companyA, Kind: feed.KindText, Status: feed.StatusPublished}
	for _, id := range []uuid.UUID{draft, other, text, uuid.NewV7()} {
		withPost := in
		withPost.OpportunityPostID = &id
		_, err := e.svc.Create(ctx, cmm, e.companyA, withPost)
		if !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("post %s: %v, want 422", id, err)
		}
	}
	withPost := in
	withPost.OpportunityPostID = &pub
	v, err := e.svc.Create(ctx, cmm, e.companyA, withPost)
	if err != nil {
		t.Fatal(err)
	}
	if v.Request.Via != messaging.ViaOpportunityPost || *v.Request.OpportunityPostID != pub {
		t.Fatalf("request = %+v", v.Request)
	}
	mine, err := e.svc.GetMine(ctx, actor(t, "student"), v.Request.ID)
	if err != nil || mine.Post == nil || mine.Post.Title != "Staj ilanı" || mine.Company.Name != "Şirket A" {
		t.Fatalf("student view = %+v, %v", mine, err)
	}
}

// TestAccept_GrantsL3InSameTx (01 §6.3 "İletişim talebi kabulü"): the
// acceptance, the level 3 grant (source contact_request, the request's ID)
// and contact_request.closed share one transaction; a failing grant rolls
// the acceptance back (the request stays pending, no event); the company
// then sees the student's user ID (04 §2.9).
func TestAccept_GrantsL3InSameTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	stu := actor(t, "student")
	v := e.send(t, "student")
	if v.Request.StudentUserID != stu.UserID || ToCompany(v).StudentUserID != "" {
		t.Fatalf("pending request shows the student: %+v", ToCompany(v))
	}

	e.students.failGrant = students.ErrMinorRestricted
	_, err := e.svc.Respond(ctx, stu, v.Request.ID, accept)
	problemtest.WantErr(t, err, students.ErrMinorRestricted)
	got, _ := e.svc.GetMine(ctx, stu, v.Request.ID)
	if got.Status != messaging.StatusPending || got.Request.RespondedAt != nil || len(e.students.granted()) != 0 ||
		len(e.outbox.of(domainevent.TopicContactRequestClosed)) != 0 {
		t.Fatalf("a failed grant left the acceptance: %+v, grants %v", got, e.students.granted())
	}

	e.students.failGrant = nil
	got, err = e.svc.Respond(ctx, stu, v.Request.ID, accept)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != messaging.StatusAccepted || got.Request.RespondedAt == nil {
		t.Fatalf("after accept: %+v", got)
	}
	gs := e.students.granted()
	if len(gs) != 1 || gs[0].student != stu.UserID || gs[0].company != e.companyA ||
		gs[0].src != students.SourceContactRequest || gs[0].srcID != v.Request.ID {
		t.Fatalf("grants = %+v", gs)
	}
	closed := e.outbox.of(domainevent.TopicContactRequestClosed)
	if len(closed) != 1 || closed[0].tx != gs[0].tx {
		t.Fatalf("closed events %+v not in the grant's transaction %d", closed, gs[0].tx)
	}
	p, _ := domainevent.Decode[domainevent.ContactRequestClosed](closed[0].env)
	if p.Outcome != "accepted" || p.ContactRequestID != v.Request.ID || p.OrgID != e.companyA {
		t.Fatalf("closed payload = %+v", p)
	}
	if rec := e.audit.of(authz.MessagingContactRequestRespond); len(rec) != 1 || rec[0].tx != gs[0].tx || rec[0].ev.OrgID != nil {
		t.Fatalf("respond audit = %+v (the student's record has no org scope)", rec)
	}

	// The same answer again changes nothing; the other answer is 409.
	if _, err := e.svc.Respond(ctx, stu, v.Request.ID, accept); err != nil {
		t.Fatalf("accept again: %v", err)
	}
	if len(e.students.granted()) != 1 {
		t.Fatal("a repeated acceptance granted again")
	}
	_, err = e.svc.Respond(ctx, stu, v.Request.ID, decline)
	problemtest.WantProblem(t, err, 409, "messaging.invalid_state")

	cv, err := e.svc.GetSent(ctx, actor(t, "company_admin"), e.companyA, v.Request.ID)
	if err != nil || ToCompany(cv).StudentUserID != stu.UserID.String() || cv.Status != messaging.StatusAccepted {
		t.Fatalf("company view after accept = %+v, %v", ToCompany(cv), err)
	}
}

// TestAccept_NeedsVerification_DeclineFree (03 §2.3; BE-077): a student
// whose verification lapsed cannot accept (403
// students.verification_required, nothing written) but may decline.
func TestAccept_NeedsVerification_DeclineFree(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.send(t, "student")
	lapsed := actor(t, "student")
	lapsed.StudentStatus, lapsed.UniversityID = authz.StudentLapsed, nil
	_, err := e.svc.Respond(ctx, lapsed, v.Request.ID, accept)
	problemtest.WantErr(t, err, students.ErrVerificationRequired)
	problemtest.WantProblem(t, err, 403, "students.verification_required")
	if len(e.students.granted()) != 0 {
		t.Fatal("granted without a verification")
	}
	got, err := e.svc.Respond(ctx, lapsed, v.Request.ID, decline)
	if err != nil || got.Status != messaging.StatusDeclined {
		t.Fatalf("decline = %+v, %v", got, err)
	}
	_, err = e.svc.Respond(ctx, lapsed, v.Request.ID, messaging.RespondInput{Decision: "maybe"})
	problemtest.WantErr(t, err, problem.ErrValidation)
}

// TestDecline_NoReasonExposedToCompany: a decline has no reason anywhere:
// the company's view, the event and the notice carry only the outcome; the
// student's record is not in the company's audit scope (no name of who
// declined).
func TestDecline_NoReasonExposedToCompany(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.send(t, "student")
	if _, err := e.svc.Respond(ctx, actor(t, "student"), v.Request.ID, decline); err != nil {
		t.Fatal(err)
	}
	cv, err := e.svc.GetSent(ctx, actor(t, "company_member"), e.companyA, v.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	dto := ToCompany(cv)
	b, _ := json.Marshal(dto)
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	for k := range fields {
		if strings.Contains(k, "reason") || k == "student_user_id" {
			t.Fatalf("company view of a decline has %q: %s", k, b)
		}
	}
	if dto.Status != "declined" || dto.AnonID == "" {
		t.Fatalf("company view = %s", b)
	}
	closed := e.outbox.of(domainevent.TopicContactRequestClosed)
	if len(closed) != 1 || strings.Contains(string(closed[0].env.Payload), "reason") {
		t.Fatalf("closed event = %+v", closed)
	}
	for _, rec := range e.audit.of(authz.MessagingContactRequestRespond) {
		if rec.ev.OrgID != nil {
			t.Fatalf("the decline is in the company's audit scope: %+v", rec.ev)
		}
	}
	if err := e.mod.HandleClosed(ctx, closed[0].env); err != nil {
		t.Fatal(err)
	}
	sent := e.sender.on(notify.ChannelInApp)
	if len(sent) != 1 || sent[0].Template != "messaging.contact_request_declined" ||
		*sent[0].UserID != authztest.ID("company_member") || len(sent[0].Data) != 1 {
		t.Fatalf("notices = %+v", sent)
	}
}

// TestWithdraw_OnlyPending_NoRefund (04 §2.9): a pending request is
// withdrawn without giving the unit back; withdrawing again changes
// nothing; an answered request is 409; the student sees it withdrawn.
func TestWithdraw_OnlyPending_NoRefund(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cmm := actor(t, "company_member")
	v := e.send(t, "student")
	left := e.ent.left(e.companyA)
	w, err := e.svc.Withdraw(ctx, cmm, e.companyA, v.Request.ID)
	if err != nil || w.Status != messaging.StatusWithdrawn {
		t.Fatalf("withdraw = %+v, %v", w, err)
	}
	if e.ent.left(e.companyA) != left || len(e.ent.consumed()) != 1 {
		t.Fatal("the unit was given back")
	}
	if _, err := e.svc.Withdraw(ctx, cmm, e.companyA, v.Request.ID); err != nil {
		t.Fatalf("withdraw again: %v", err)
	}
	if n := len(e.audit.of(authz.MessagingContactRequestWithdraw)); n != 1 {
		t.Fatalf("withdraw audit records = %d, want 1", n)
	}
	_, err = e.svc.Respond(ctx, actor(t, "student"), v.Request.ID, accept)
	problemtest.WantProblem(t, err, 409, "messaging.invalid_state")

	a := e.send(t, "student_granted")
	if _, err := e.svc.Respond(ctx, actor(t, "student_granted"), a.Request.ID, accept); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Withdraw(ctx, cmm, e.companyA, a.Request.ID)
	problemtest.WantProblem(t, err, 409, "messaging.invalid_state")

	// Another company's request is 404; a suspended company may not write.
	_, err = e.svc.Withdraw(ctx, actor(t, "other_company_member"), e.companyB, a.Request.ID)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	b, err := e.svc.Create(ctx, actor(t, "other_company_member"), e.companyB, request(e.companyB, "student"))
	if err != nil {
		t.Fatal(err)
	}
	e.orgs.status(e.companyB, orgs.StatusSuspended)
	_, err = e.svc.Withdraw(ctx, actor(t, "other_company_member"), e.companyB, b.Request.ID)
	problemtest.WantErr(t, err, orgs.ErrOrgSuspended)
	if _, err := e.svc.GetSent(ctx, actor(t, "other_company_member"), e.companyB, b.Request.ID); err != nil {
		t.Fatalf("a suspended company still reads: %v", err)
	}
}

// TestExpire_After14Days_NoRefund (01 §7): a request unanswered for 14 days
// reads as expired at once, cannot be answered, and
// messaging.expire_requests closes it (contact_request.closed "expired", a
// system audit record) without giving the unit back; a fresh request is
// left alone.
func TestExpire_After14Days_NoRefund(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	old := e.send(t, "student")
	e.clk.Advance(messaging.RequestTTL - time.Hour)
	fresh := e.send(t, "student_granted")
	e.clk.Advance(time.Hour)

	got, err := e.svc.GetMine(ctx, actor(t, "student"), old.Request.ID)
	if err != nil || got.Status != messaging.StatusExpired {
		t.Fatalf("before the job: %+v, %v", got, err)
	}
	_, err = e.svc.Respond(ctx, actor(t, "student"), old.Request.ID, accept)
	problemtest.WantProblem(t, err, 409, "messaging.invalid_state")
	left := e.ent.left(e.companyA)

	if err := e.mod.ExpireDue(ctx); err != nil {
		t.Fatal(err)
	}
	row, _ := e.store.GetForStudent(ctx, authztest.ID("student"), old.Request.ID)
	if row.Status != messaging.StatusExpired || row.RespondedAt != nil {
		t.Fatalf("expired row = %+v", row)
	}
	if fr, _ := e.store.GetForStudent(ctx, authztest.ID("student_granted"), fresh.Request.ID); fr.Status != messaging.StatusPending {
		t.Fatalf("a fresh request expired: %+v", fr)
	}
	closed := e.outbox.of(domainevent.TopicContactRequestClosed)
	if len(closed) != 1 {
		t.Fatalf("closed events = %d", len(closed))
	}
	if p, _ := domainevent.Decode[domainevent.ContactRequestClosed](closed[0].env); p.Outcome != "expired" {
		t.Fatalf("outcome = %q", p.Outcome)
	}
	rec := e.audit.of(authz.MessagingContactRequestExpire)
	if len(rec) != 1 || rec[0].ev.SystemJob != messaging.ExpireJobName || rec[0].ev.ActorUserID != nil {
		t.Fatalf("expire audit = %+v", rec)
	}
	if e.ent.left(e.companyA) != left || len(e.ent.consumed()) != 2 {
		t.Fatal("expiry gave a unit back")
	}
	// A second run finds nothing; the company may ask again (no 90-day rule).
	if err := e.mod.ExpireDue(ctx); err != nil || len(e.outbox.of(domainevent.TopicContactRequestClosed)) != 1 {
		t.Fatalf("second run: %v", err)
	}
	if _, err := e.svc.Create(ctx, actor(t, "company_member"), e.companyA, request(e.companyA, "student")); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

// TestCreate_ExpiresDuePendingOnTheSpot: a pending request past its 14
// days that the job has not closed yet does not block a new one: it is
// expired in the new request's transaction.
func TestCreate_ExpiresDuePendingOnTheSpot(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	old := e.send(t, "student")
	e.clk.Advance(messaging.RequestTTL)
	v := e.send(t, "student")
	if v.Request.ID == old.Request.ID {
		t.Fatal("no new request")
	}
	row, _ := e.store.GetForStudent(t.Context(), authztest.ID("student"), old.Request.ID)
	closed := e.outbox.of(domainevent.TopicContactRequestClosed)
	if row.Status != messaging.StatusExpired || len(closed) != 1 {
		t.Fatalf("old = %+v, closed events %d", row, len(closed))
	}
}

// TestBlockCompany_DeclinesPending (04 §2.9): blocking declines the
// company's pending requests to the student in the same transaction (the
// company is told "declined" like any decline), leaves other companies'
// alone, is idempotent and has no org scope in the audit.
func TestBlockCompany_DeclinesPending(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	stu := actor(t, "student")
	a := e.send(t, "student")
	b, err := e.svc.Create(ctx, actor(t, "other_company_member"), e.companyB, request(e.companyB, "student"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Block(ctx, stu, e.companyA); err != nil {
		t.Fatal(err)
	}
	ra, _ := e.store.GetForStudent(ctx, stu.UserID, a.Request.ID)
	rb, _ := e.store.GetForStudent(ctx, stu.UserID, b.Request.ID)
	if ra.Status != messaging.StatusDeclined || ra.RespondedAt == nil || rb.Status != messaging.StatusPending {
		t.Fatalf("after block: a=%s b=%s", ra.Status, rb.Status)
	}
	closed := e.outbox.of(domainevent.TopicContactRequestClosed)
	if len(closed) != 1 {
		t.Fatalf("closed events = %d", len(closed))
	}
	if p, _ := domainevent.Decode[domainevent.ContactRequestClosed](closed[0].env); p.Outcome != "declined" || p.ContactRequestID != a.Request.ID {
		t.Fatalf("closed = %+v", p)
	}
	rec := e.audit.of(authz.MessagingBlockManage)
	if len(rec) != 1 || rec[0].ev.OrgID != nil || rec[0].tx != closed[0].tx {
		t.Fatalf("block audit = %+v", rec)
	}
	// Idempotent: nothing new.
	if err := e.svc.Block(ctx, stu, e.companyA); err != nil || len(e.audit.of(authz.MessagingBlockManage)) != 1 {
		t.Fatalf("block again: %v", err)
	}
	page, err := e.svc.ListBlocks(ctx, stu, nil, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Company.Name != "Şirket A" {
		t.Fatalf("blocks = %+v, %v", page, err)
	}
	// Only a company can be blocked; another user's blocks are their own.
	problemtest.WantErr(t, e.svc.Block(ctx, stu, e.clubA), problem.ErrNotFound)
	problemtest.WantErr(t, e.svc.Block(ctx, stu, uuid.NewV7()), problem.ErrNotFound)
	if other, _ := e.svc.ListBlocks(ctx, actor(t, "student_granted"), nil, 10); len(other.Items) != 0 {
		t.Fatalf("another student's blocks = %+v", other)
	}
	if err := e.svc.Unblock(ctx, stu, e.companyA); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Unblock(ctx, stu, e.companyA); err != nil {
		t.Fatalf("unblock again: %v", err)
	}
	if after, _ := e.svc.ListBlocks(ctx, stu, nil, 10); len(after.Items) != 0 {
		t.Fatalf("blocks after unblock = %+v", after)
	}
}

// TestNotification_NoMessageBody (01 §6.2): contact_request.created holds
// IDs only; the student's notice is in-app with a fixed text, the company as
// its source, the request as the link and data, never the subject or the
// message; a request withdrawn before the task runs is not announced.
func TestNotification_NoMessageBody(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	in := request(e.companyA, "student")
	in.Subject, in.Message = "Gizli konu satırı", "Bu mesaj gövdesi bildirime asla girmemeli."
	v, err := e.svc.Create(ctx, actor(t, "company_member"), e.companyA, in)
	if err != nil {
		t.Fatal(err)
	}
	created := e.outbox.of(domainevent.TopicContactRequestCreated)
	if len(created) != 1 {
		t.Fatalf("created events = %d", len(created))
	}
	payload := string(created[0].env.Payload)
	if strings.Contains(payload, in.Subject) || strings.Contains(payload, in.Message) || strings.Contains(payload, in.AnonID) {
		t.Fatalf("event payload leaks the request: %s", payload)
	}
	if err := e.mod.HandleCreated(ctx, created[0].env); err != nil {
		t.Fatal(err)
	}
	if err := e.mod.HandleCreated(ctx, created[0].env); err != nil { // a redelivery
		t.Fatal(err)
	}
	sent := e.sender.on(notify.ChannelInApp)
	if len(sent) != 1 || len(e.sender.all()) != 3 {
		t.Fatalf("in-app notices = %d, want 1 (and push, e-mail: %d in all)", len(sent), len(e.sender.all()))
	}
	m := sent[0]
	if m.Template != "messaging.contact_request_received" || *m.UserID != authztest.ID("student") ||
		m.SourceOrgID == nil || *m.SourceOrgID != e.companyA || m.LinkPath.String() != "/me/contact-requests/"+v.Request.ID.String() {
		t.Fatalf("notice = %+v", m)
	}
	b, _ := json.Marshal(m.Data)
	if string(b) != `{"contact_request_id":"`+v.Request.ID.String()+`"}` || strings.Contains(m.DedupeKey, in.Subject) {
		t.Fatalf("notice data = %s", b)
	}

	w := e.send(t, "student_granted")
	if _, err := e.svc.Withdraw(ctx, actor(t, "company_member"), e.companyA, w.Request.ID); err != nil {
		t.Fatal(err)
	}
	for _, ev := range e.outbox.of(domainevent.TopicContactRequestCreated)[1:] {
		if err := e.mod.HandleCreated(ctx, ev.env); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.sender.all()) != 3 {
		t.Fatal("a withdrawn request was announced")
	}
}

// TestClosedNotice_Recipients: the sender is told while a member of the
// company; once the sender left, the company's owner and admins.
func TestClosedNotice_Recipients(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.send(t, "student")
	if _, err := e.svc.Respond(ctx, actor(t, "student"), v.Request.ID, accept); err != nil {
		t.Fatal(err)
	}
	e.orgs.leave(e.companyA, authztest.ID("company_member"))
	closed := e.outbox.of(domainevent.TopicContactRequestClosed)
	if err := e.mod.HandleClosed(ctx, closed[0].env); err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]string{}
	for _, m := range e.sender.on(notify.ChannelInApp) {
		got[*m.UserID] = m.Template
		if m.LinkPath.String() != "/orgs/"+e.companyA.String()+"/contact-requests/"+v.Request.ID.String() {
			t.Fatalf("link = %s", m.LinkPath.String())
		}
	}
	want := map[uuid.UUID]string{
		authztest.ID("company_admin"): "messaging.contact_request_accepted", authztest.ID("company_owner_a"): "messaging.contact_request_accepted",
	}
	if len(got) != len(want) {
		t.Fatalf("recipients = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("recipients = %v, want %v", got, want)
		}
	}
}

// TestRead_IDOR (03 §7.2): another student's request, another company's
// request and a missing one are the same 404; lists hold one's own only.
func TestRead_IDOR(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.send(t, "student")
	_, err := e.svc.GetMine(ctx, actor(t, "student_granted"), v.Request.ID)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.Respond(ctx, actor(t, "student_granted"), v.Request.ID, accept)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.GetSent(ctx, actor(t, "other_company_member"), e.companyB, v.Request.ID)
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.GetSent(ctx, actor(t, "company_member"), e.companyA, uuid.NewV7())
	problemtest.WantErr(t, err, problem.ErrNotFound)
	_, err = e.svc.GetSent(ctx, actor(t, "moderator"), e.companyA, v.Request.ID)
	problemtest.WantErr(t, err, authz.ErrForbidden)

	mine, err := e.svc.ListMine(ctx, actor(t, "student"), messaging.ListQuery{})
	if err != nil || len(mine.Items) != 1 || mine.Items[0].Company.Name != "Şirket A" {
		t.Fatalf("student list = %+v, %v", mine, err)
	}
	if other, _ := e.svc.ListMine(ctx, actor(t, "student_granted"), messaging.ListQuery{}); len(other.Items) != 0 {
		t.Fatalf("another student's list = %+v", other)
	}
	sent, err := e.svc.ListSent(ctx, actor(t, "company_admin"), e.companyA, messaging.ListQuery{Status: messaging.StatusPending})
	if err != nil || len(sent.Items) != 1 || sent.Items[0].AnonID != anonOf(e.companyA, authztest.ID("student")) {
		t.Fatalf("company list = %+v, %v", sent, err)
	}
	if b, _ := e.svc.ListSent(ctx, actor(t, "other_company_member"), e.companyB, messaging.ListQuery{}); len(b.Items) != 0 {
		t.Fatalf("company_b sees company_a's requests: %+v", b)
	}
	_, err = e.svc.ListSent(ctx, actor(t, "company_member"), e.companyA, messaging.ListQuery{Status: "read"})
	problemtest.WantErr(t, err, problem.ErrValidation)
}

// TestList_PagingAndStatusFilter: newest first, keyset pages, the
// effective status filter (a pending request past its days lists as
// expired).
func TestList_PagingAndStatusFilter(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cma := actor(t, "company_admin")
	first := e.send(t, "student")
	e.clk.Advance(time.Minute)
	second := e.send(t, "student_granted")
	p1, err := e.svc.ListSent(ctx, cma, e.companyA, messaging.ListQuery{Limit: 1})
	if err != nil || len(p1.Items) != 1 || p1.Items[0].Request.ID != second.Request.ID || p1.Next == nil {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	p2, err := e.svc.ListSent(ctx, cma, e.companyA, messaging.ListQuery{Limit: 1, After: p1.Next})
	if err != nil || len(p2.Items) != 1 || p2.Items[0].Request.ID != first.Request.ID || p2.Next != nil {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	e.clk.Advance(messaging.RequestTTL - 30*time.Second)
	exp, _ := e.svc.ListSent(ctx, cma, e.companyA, messaging.ListQuery{Status: messaging.StatusExpired})
	pend, _ := e.svc.ListSent(ctx, cma, e.companyA, messaging.ListQuery{Status: messaging.StatusPending})
	if len(exp.Items) != 1 || exp.Items[0].Request.ID != first.Request.ID || len(pend.Items) != 1 ||
		pend.Items[0].Request.ID != second.Request.ID {
		t.Fatalf("expired %+v, pending %+v", exp, pend)
	}
}

// ToCompany is the company DTO of v (the HTTP view).
func ToCompany(v messaging.CompanyView) messaging.ContactRequestCompanyDTO {
	return messaging.ToCompanyDTO(v)
}
