package orgs_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
)

// flaggedCompany registers and verifies a company, then its owner replaces
// the VKN (with the proof): the company is active, verified and waits for a
// tax identifier review (BE-206). It returns the view after the change, the
// owner and the new VKN.
func (e *env) flaggedCompany(t *testing.T, name string) (orgs.OrgView, authz.Actor, string) {
	t.Helper()
	ctx := t.Context()
	v, owner := e.company(t, name)
	if _, err := e.svc.AdminVerifyOrg(ctx, pad(t), v.Org.ID, ""); err != nil {
		t.Fatal(err)
	}
	cur, err := e.svc.GetOrg(ctx, owner, v.Org.ID)
	if err != nil {
		t.Fatal(err)
	}
	vkn := otherVKN(t)
	got, err := e.svc.UpdateOrg(ctx, owner, v.Org.ID, cur.ETag, orgs.OrgPatch{TaxNo: &vkn, Proof: proof(goodPassword)})
	if err != nil || got.Company.TaxIDReviewRequestedAt == nil || got.Org.Status != orgs.StatusActive {
		t.Fatalf("flaggedCompany: %+v, %v", got.Company, err)
	}
	return got, owner, vkn
}

// adminETag is the org's ETag as a platform admin reads it.
func (e *env) adminETag(t *testing.T, id uuid.UUID) string {
	t.Helper()
	cur, err := e.svc.GetOrg(t.Context(), pad(t), id)
	if err != nil {
		t.Fatal(err)
	}
	return cur.ETag
}

// TestTaxIDReview_ApproveKeepsTheCompany (BE-206, orgs.org.verify): only a
// platform admin approves (403 for the other staff, before anything is
// read); the company as the admin saw it (If-Match: 428 missing, 412 after
// a newer identifier); a company without a pending review is 409. The
// approval clears the flag and changes nothing else; org.review_decided
// (tax_id_approved) and the audit (the note, no number) follow; the reviewer
// sees the number unmasked. A second approval is 409.
func TestTaxIDReview_ApproveKeepsTheCompany(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v, owner, vkn := e.flaggedCompany(t, "Onay Şirketi")
	id := v.Org.ID
	etag := e.adminETag(t, id)

	for _, p := range []string{"moderator", "support", "finance_admin"} {
		_, err := e.svc.AdminApproveTaxIDChange(ctx, persona(t, p), id, etag, "")
		problemtest.WantErr(t, err, authz.ErrForbidden)
	}
	_, err := e.svc.AdminApproveTaxIDChange(ctx, owner, id, etag, "")
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.AdminApproveTaxIDChange(ctx, pad(t), id, "", "")
	problemtest.WantProblem(t, err, 428, "precondition_required")
	_, err = e.svc.AdminApproveTaxIDChange(ctx, pad(t), id, `"0"`, "")
	problemtest.WantProblem(t, err, 412, "precondition_failed")
	_, err = e.svc.AdminApproveTaxIDChange(ctx, pad(t), id, etag, strings.Repeat("a", 501))
	wantField(t, err, "body.note", "")
	_, err = e.svc.AdminApproveTaxIDChange(ctx, pad(t), uuid.NewV7(), etag, "")
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	plain, _ := e.company(t, "Sade Şirket")
	if _, err := e.svc.AdminVerifyOrg(ctx, pad(t), plain.Org.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AdminApproveTaxIDChange(ctx, pad(t), plain.Org.ID, e.adminETag(t, plain.Org.ID), "")
	problemtest.WantErr(t, err, orgs.ErrInvalidState)
	club := e.club(t, "Vergi Kulübü", orgs.JoinOpen, false)
	_, err = e.svc.AdminApproveTaxIDChange(ctx, pad(t), club, e.adminETag(t, club), "")
	problemtest.WantErr(t, err, orgs.ErrInvalidState)

	decided := len(eventsOf[domainevent.OrgReviewDecided](t, e, domainevent.TopicOrgReviewDecided))
	got, err := e.svc.AdminApproveTaxIDChange(ctx, pad(t), id, etag, "GİB sorgusu eşleşti")
	if err != nil {
		t.Fatal(err)
	}
	if got.Org.Status != orgs.StatusActive || !got.Org.Verified() || got.Company.TaxIDReviewRequestedAt != nil ||
		*got.Company.TaxNo != vkn || !got.FullTaxNo || got.ETag != etag {
		t.Fatalf("after the approval = %+v, %+v (full %v)", got.Org, got.Company, got.FullTaxNo)
	}
	evs := eventsOf[domainevent.OrgReviewDecided](t, e, domainevent.TopicOrgReviewDecided)
	if len(evs) != decided+1 || evs[len(evs)-1].Outcome != domainevent.OrgReviewTaxIDApproved || evs[len(evs)-1].OrgID != id {
		t.Fatalf("org.review_decided = %+v", evs)
	}
	rows := e.audit.Of(authz.OrgsOrgVerify)
	last := rows[len(rows)-1]
	if last.Reason != "GİB sorgusu eşleşti" || last.Metadata["tax_id_review"] != "approved" || last.Metadata["waited_seconds"] == nil ||
		strings.Contains(fmt.Sprint(last.Metadata), vkn) {
		t.Fatalf("audit = %+v %+v", last.Reason, last.Metadata)
	}
	_, err = e.svc.AdminApproveTaxIDChange(ctx, pad(t), id, etag, "")
	problemtest.WantErr(t, err, orgs.ErrInvalidState)

	// A newer identifier than the admin read is never approved unseen.
	v2, owner2, _ := e.flaggedCompany(t, "Yarış Şirketi")
	seen := e.adminETag(t, v2.Org.ID)
	tckn := testTCKN
	if _, err := e.svc.UpdateOrg(ctx, owner2, v2.Org.ID, v2.ETag, orgs.OrgPatch{TCKN: &tckn, Proof: proof(goodPassword)}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AdminApproveTaxIDChange(ctx, pad(t), v2.Org.ID, seen, "")
	problemtest.WantProblem(t, err, 412, "precondition_failed")
}

// TestTaxIDReview_RejectSuspendsTheCompany (BE-206, orgs.org.suspend, R): a
// reason (422) and the proof (403 identity.reauth_required, reauth_failed with
// a denied record) are required; the pending review and If-Match are checked
// before the proof. The rejection clears the flag and suspends the active
// company: org.updated (status), org.review_decided (tax_id_rejected), the
// audit with the reason and the transition. Its members' writes are then 403
// orgs.org_suspended; a platform admin corrects the identifier (R, no new
// review) and reactivates it.
func TestTaxIDReview_RejectSuspendsTheCompany(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v, owner, vkn := e.flaggedCompany(t, "Ret Şirketi")
	id := v.Org.ID
	etag := e.adminETag(t, id)
	const reason = "VKN şirket unvanıyla eşleşmiyor"

	_, err := e.svc.AdminRejectTaxIDChange(ctx, persona(t, "support"), id, etag, reason, proof(goodPassword))
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.AdminRejectTaxIDChange(ctx, pad(t), id, etag, "x", proof(goodPassword))
	wantField(t, err, "body.reason", "")
	calls := e.reauth.calls
	_, err = e.svc.AdminRejectTaxIDChange(ctx, pad(t), id, `"0"`, reason, proof(goodPassword))
	problemtest.WantProblem(t, err, 412, "precondition_failed")
	plain, _ := e.company(t, "Bekleyen Şirket")
	_, err = e.svc.AdminRejectTaxIDChange(ctx, pad(t), plain.Org.ID, e.adminETag(t, plain.Org.ID), reason, proof(goodPassword))
	problemtest.WantErr(t, err, orgs.ErrInvalidState) // pending_review: nothing flagged
	if e.reauth.calls != calls {
		t.Fatal("a proof was checked before the review and If-Match")
	}
	_, err = e.svc.AdminRejectTaxIDChange(ctx, pad(t), id, etag, reason, identity.ReauthProof{})
	problemtest.WantErr(t, err, identity.ErrReauthRequired)
	_, err = e.svc.AdminRejectTaxIDChange(ctx, pad(t), id, etag, reason, proof("yanlis-parola-1"))
	problemtest.WantErr(t, err, identity.ErrReauthFailed)
	if rows := e.audit.Rows(authz.OrgsOrgSuspend); len(rows) != 2 || rows[1].Metadata["tax_id_review"] != "rejected" {
		t.Fatalf("denied records = %+v", rows)
	}

	n := len(e.out.Events())
	got, err := e.svc.AdminRejectTaxIDChange(ctx, pad(t), id, etag, reason, proof(goodPassword))
	if err != nil {
		t.Fatal(err)
	}
	if got.Org.Status != orgs.StatusSuspended || got.Company.TaxIDReviewRequestedAt != nil || *got.Company.TaxNo != vkn ||
		!got.FullTaxNo {
		t.Fatalf("after the rejection = %+v, %+v", got.Org, got.Company)
	}
	var topics []string
	for _, ev := range e.out.Events()[n:] {
		topics = append(topics, ev.Topic)
	}
	if !slices.Equal(topics, []string{domainevent.TopicOrgUpdated, domainevent.TopicOrgReviewDecided}) {
		t.Fatalf("events = %v", topics)
	}
	evs := eventsOf[domainevent.OrgReviewDecided](t, e, domainevent.TopicOrgReviewDecided)
	if evs[len(evs)-1].Outcome != domainevent.OrgReviewTaxIDRejected {
		t.Fatalf("org.review_decided = %+v", evs[len(evs)-1])
	}
	rows := e.audit.Of(authz.OrgsOrgSuspend)
	last := rows[len(rows)-1]
	if last.Reason != reason || last.Metadata["tax_id_review"] != "rejected" || last.Metadata["from"] != "active" ||
		last.Metadata["to"] != "suspended" || last.Metadata["reauth"] != "password" || strings.Contains(fmt.Sprint(last.Metadata), vkn) {
		t.Fatalf("audit = %+v %+v", last.Reason, last.Metadata)
	}
	_, err = e.svc.AdminRejectTaxIDChange(ctx, pad(t), id, e.adminETag(t, id), reason, proof(goodPassword))
	problemtest.WantErr(t, err, orgs.ErrInvalidState)

	city := "İzmir"
	_, err = e.svc.UpdateOrg(ctx, owner, id, e.adminETag(t, id), orgs.OrgPatch{City: &city})
	problemtest.WantErr(t, err, orgs.ErrOrgSuspended)

	// The way back: a platform admin corrects the identifier, then reactivates.
	fixed := testVKN
	cor, err := e.svc.UpdateOrg(ctx, pad(t), id, e.adminETag(t, id), orgs.OrgPatch{TaxNo: &fixed, Proof: proof(goodPassword)})
	if err != nil || *cor.Company.TaxNo != fixed || cor.Company.TaxIDReviewRequestedAt != nil {
		t.Fatalf("the platform admin's correction = %+v, %v", cor.Company, err)
	}
	back, err := e.svc.AdminReactivateOrg(ctx, pad(t), id, "Doğru VKN girildi", proof(goodPassword))
	if err != nil || back.Org.Status != orgs.StatusActive {
		t.Fatalf("reactivated = %+v, %v", back.Org, err)
	}

	// A suspended company's pending review: the rejection keeps it suspended.
	v2, _, _ := e.flaggedCompany(t, "Askıdaki Şirket")
	if _, err := e.svc.AdminSuspendOrg(ctx, pad(t), v2.Org.ID, "Şikâyet incelemesi", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	n = len(e.out.Events())
	got, err = e.svc.AdminRejectTaxIDChange(ctx, pad(t), v2.Org.ID, e.adminETag(t, v2.Org.ID), reason, proof(goodPassword))
	if err != nil || got.Org.Status != orgs.StatusSuspended || got.Company.TaxIDReviewRequestedAt != nil {
		t.Fatalf("rejection of a suspended company = %+v, %v", got.Org, err)
	}
	if evs := e.out.Events()[n:]; len(evs) != 1 || evs[0].Topic != domainevent.TopicOrgReviewDecided {
		t.Fatalf("events of a suspended company's rejection = %+v", evs)
	}
	rows = e.audit.Of(authz.OrgsOrgSuspend)
	if last := rows[len(rows)-1]; last.Metadata["from"] != nil {
		t.Fatalf("a transition was audited for a suspended company: %+v", last.Metadata)
	}
}

// TestTaxIDReview_ClosedCompanyHasNothingToDecide (BE-206): a flagged company
// that closes leaves the queue: the list, the decisions (409) and the DTOs.
func TestTaxIDReview_ClosedCompanyHasNothingToDecide(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v, owner, _ := e.flaggedCompany(t, "Kapanan Şirket")
	if _, err := e.svc.CloseOrg(ctx, owner, v.Org.ID, "Faaliyet sona erdi", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	etag := e.adminETag(t, v.Org.ID)
	_, err := e.svc.AdminApproveTaxIDChange(ctx, pad(t), v.Org.ID, etag, "")
	problemtest.WantErr(t, err, orgs.ErrInvalidState)
	_, err = e.svc.AdminRejectTaxIDChange(ctx, pad(t), v.Org.ID, etag, "Kapalı şirket", proof(goodPassword))
	problemtest.WantErr(t, err, orgs.ErrInvalidState)
	p, err := e.svc.AdminListOrgs(ctx, pad(t), orgs.AdminQuery{TaxIDReview: true})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("queue = %+v, %v", p.Items, err)
	}
	cur, err := e.svc.GetOrg(ctx, pad(t), v.Org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d := orgs.ToOrgDTO(cur).Company; d.TaxIDReviewRequestedAt != nil {
		t.Fatalf("a closed company shows a pending review: %+v", d)
	}
}

// TestAdminListOrgs_TaxIDReviewFilter (BE-206): tax_id_review keeps the
// flagged companies (any status but closed), with the other filters; the
// admin list shows the review's time, never the number; moderator and
// support read the queue (orgs.org.list_admin).
func TestAdminListOrgs_TaxIDReviewFilter(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	waiting, _, vkn := e.flaggedCompany(t, "Kuyruk Şirketi")
	suspended, _, _ := e.flaggedCompany(t, "Askıda Kuyruk")
	if _, err := e.svc.AdminSuspendOrg(ctx, pad(t), suspended.Org.ID, "Şikâyet incelemesi", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	plain, _ := e.company(t, "Kuyruksuz Şirket")
	if _, err := e.svc.AdminVerifyOrg(ctx, pad(t), plain.Org.ID, ""); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"moderator", "support", "platform_admin"} {
		page, err := e.svc.AdminListOrgs(ctx, persona(t, p), orgs.AdminQuery{TaxIDReview: true})
		if err != nil || len(page.Items) != 2 {
			t.Fatalf("%s: queue = %d items, %v", p, len(page.Items), err)
		}
	}
	page, err := e.svc.AdminListOrgs(ctx, pad(t), orgs.AdminQuery{TaxIDReview: true, Status: orgs.StatusActive})
	if err != nil || len(page.Items) != 1 || page.Items[0].Org.ID != waiting.Org.ID {
		t.Fatalf("active queue = %+v, %v", page.Items, err)
	}
	d := orgs.ToAdminDTO(page.Items[0])
	if d.Company == nil || d.Company.TaxIDReviewRequestedAt == nil || !d.Company.TaxIDReviewRequestedAt.Equal(*waiting.Company.TaxIDReviewRequestedAt) {
		t.Fatalf("admin DTO = %+v", d.Company)
	}
	body, err := json.Marshal(d)
	if err != nil || strings.Contains(string(body), vkn) || !strings.Contains(string(body), `"tax_id_review_requested_at"`) {
		t.Fatalf("admin list body = %s, %v", body, err)
	}
	all, err := e.svc.AdminListOrgs(ctx, pad(t), orgs.AdminQuery{Kind: orgs.KindCompany})
	if err != nil || len(all.Items) != 3 {
		t.Fatalf("without the filter = %d items, %v", len(all.Items), err)
	}
	for _, it := range all.Items {
		if it.Org.ID == plain.Org.ID && orgs.ToAdminDTO(it).Company.TaxIDReviewRequestedAt != nil {
			t.Fatal("an unflagged company shows a review")
		}
	}
	st, err := e.store.OrgStats(ctx, t0, t0)
	if err != nil || st.TaxIDReviews != 2 {
		t.Fatalf("OrgStats.TaxIDReviews = %d, %v", st.TaxIDReviews, err)
	}
	for _, p := range []string{"dpo", "student"} {
		_, err := e.svc.AdminListOrgs(ctx, persona(t, p), orgs.AdminQuery{TaxIDReview: true})
		problemtest.WantErr(t, err, authz.ErrForbidden)
	}
}

// TestTaxIDReview_Notices (BE-206): the flag's org.review_requested tells
// every platform admin in-app, once per review, linking the admin list of
// the tax identifier reviews, unless the review was decided before the task
// ran. An approval tells the company's active admins (not a recruiter)
// in-app; a rejection tells them in-app and by e-mail, the e-mail with the
// staff's reason read from the company (BE-208; never in the in-app data),
// unless the company was reactivated meanwhile.
func TestTaxIDReview_Notices(t *testing.T) {
	t.Parallel()
	a1, a2 := uuid.NewV7(), uuid.NewV7()
	e := reviewEnv(t, a1, a2)
	ctx := t.Context()
	v, owner, _ := e.flaggedCompany(t, "Bildirim Şirketi")
	id := v.Org.ID
	admin := e.member(t, id, authz.MemberCompanyAdmin)
	rec := e.member(t, id, authz.MemberRecruiter)
	for _, a := range []authz.Actor{owner, admin, rec} {
		e.users.add(a.UserID, a.UserID.String()+"@sirket.example.test", false)
	}
	e.deliver(t, domainevent.TopicOrgReviewRequested)
	got := e.sentOf("orgs.tax_id_review_requested", notify.ChannelInApp)
	if len(got) != 2 {
		t.Fatalf("review notices %+v, want one per platform admin", got)
	}
	for _, m := range got {
		if (*m.UserID != a1 && *m.UserID != a2) || m.LinkPath != notify.AdminTaxIDReviewsLink() || *m.SourceOrgID != id ||
			len(m.Data) != 1 {
			t.Fatalf("review notice %+v", m)
		}
	}
	// The registration's event, delivered after the verification and the flag,
	// finds the same review: no registration notice, no second review notice.
	if n := len(e.sentOf("orgs.review_requested", notify.ChannelInApp)); n != 0 {
		t.Fatalf("%d registration notices for a verified company", n)
	}

	if _, err := e.svc.AdminApproveTaxIDChange(ctx, pad(t), id, e.adminETag(t, id), ""); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, domainevent.TopicOrgReviewDecided)
	want := []uuid.UUID{owner.UserID, admin.UserID}
	approved := e.sentOf("orgs.tax_id_approved", notify.ChannelInApp)
	if len(approved) != 2 || len(e.sentOf("orgs.tax_id_approved", notify.ChannelEmail)) != 0 {
		t.Fatalf("approval notices %+v", approved)
	}
	for _, m := range approved {
		if !slices.Contains(want, *m.UserID) || m.LinkPath != notify.OrgLink(id) {
			t.Fatalf("approval notice %+v", m)
		}
	}

	// A review decided before its task ran sends no request notice.
	e2 := reviewEnv(t, a1)
	v2, _, _ := e2.flaggedCompany(t, "Hızlı Karar")
	if _, err := e2.svc.AdminApproveTaxIDChange(ctx, pad(t), v2.Org.ID, e2.adminETag(t, v2.Org.ID), ""); err != nil {
		t.Fatal(err)
	}
	e2.deliver(t, domainevent.TopicOrgReviewRequested)
	if n := len(e2.sentOf("orgs.tax_id_review_requested", notify.ChannelInApp)); n != 0 {
		t.Fatalf("%d notices for a decided review", n)
	}

	// The rejection: in-app and e-mail to the company's admins; the e-mail
	// carries the reason (BE-208), the in-app data never does.
	e3 := reviewEnv(t)
	v3, owner3, _ := e3.flaggedCompany(t, "Reddedilen Şirket")
	e3.users.add(owner3.UserID, "sahip@reddedilen.example.test", false)
	const reason = "VKN vergi levhasıyla eşleşmiyor"
	if _, err := e3.svc.AdminRejectTaxIDChange(ctx, pad(t), v3.Org.ID, e3.adminETag(t, v3.Org.ID), reason, proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	e3.deliver(t, domainevent.TopicOrgReviewDecided)
	inApp, mails := e3.sentOf("orgs.tax_id_rejected", notify.ChannelInApp), e3.sentOf("orgs.tax_id_rejected", notify.ChannelEmail)
	if len(inApp) != 1 || len(mails) != 1 || *mails[0].UserID != owner3.UserID || mails[0].ToEmail != "sahip@reddedilen.example.test" ||
		mails[0].Data["org_name"] != "Reddedilen Şirket" || mails[0].Data["reason"] != reason ||
		inApp[0].LinkPath != notify.OrgLink(v3.Org.ID) {
		t.Fatalf("rejection notices in-app %+v, e-mail %+v", inApp, mails)
	}
	if strings.Contains(fmt.Sprint(inApp[0].Data), reason) {
		t.Fatal("the staff's reason reached the in-app data")
	}

	// Reactivated before the task ran: the rejection notice would be wrong.
	e4 := reviewEnv(t)
	v4, _, _ := e4.flaggedCompany(t, "Geri Dönen Şirket")
	if _, err := e4.svc.AdminRejectTaxIDChange(ctx, pad(t), v4.Org.ID, e4.adminETag(t, v4.Org.ID), "Yanlış VKN", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	if _, err := e4.svc.AdminReactivateOrg(ctx, pad(t), v4.Org.ID, "Hatalı ret", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	e4.deliver(t, domainevent.TopicOrgReviewDecided)
	if n := len(e4.sentOf("orgs.tax_id_rejected", notify.ChannelInApp)); n != 0 {
		t.Fatalf("%d rejection notices after the reactivation", n)
	}
}
