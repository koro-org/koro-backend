package orgs_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
)

// reviewEnv is an orgs module whose platform admins are admins.
func reviewEnv(t *testing.T, admins ...uuid.UUID) *env {
	t.Helper()
	return newEnv(t, func(d *orgs.Deps) {
		d.PlatformAdmins = func(context.Context) ([]uuid.UUID, error) { return admins, nil }
	})
}

// sentOf returns the messages of tmpl on ch.
func (e *env) sentOf(tmpl string, ch notify.Channel) []notify.Message {
	var out []notify.Message
	for _, m := range e.mailer.messages() {
		if m.Template == tmpl && m.Channel == ch {
			out = append(out, m)
		}
	}
	return out
}

// eventsOf returns the decoded committed events of a topic.
func eventsOf[P domainevent.Payload](t *testing.T, e *env, topic string) []P {
	t.Helper()
	var out []P
	for _, ev := range e.out.Events() {
		if ev.Topic != topic {
			continue
		}
		p, err := domainevent.Decode[P](ev)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// TestReview_NewCompanyTellsPlatformAdmins (BE-144): createCompany writes
// org.review_requested (O) with the org; the worker sends every platform
// admin one in-app notice linking the admin list of orgs awaiting review,
// once under at-least-once delivery. Once the org is decided the notice
// is not sent any more.
func TestReview_NewCompanyTellsPlatformAdmins(t *testing.T) {
	t.Parallel()
	a1, a2 := uuid.NewV7(), uuid.NewV7()
	e := reviewEnv(t, a1, a2)
	co, _ := e.company(t, "İnceleme Şirketi")
	evs := eventsOf[domainevent.OrgReviewRequested](t, e, domainevent.TopicOrgReviewRequested)
	if len(evs) != 1 || evs[0].OrgID != co.Org.ID || evs[0].OrgKind != "company" {
		t.Fatalf("org.review_requested = %+v", evs)
	}
	e.deliver(t, domainevent.TopicOrgReviewRequested)
	got := e.sentOf("orgs.review_requested", notify.ChannelInApp)
	if len(got) != 2 {
		t.Fatalf("notices %+v, want one per platform admin", got)
	}
	for _, m := range got {
		if m.UserID == nil || (*m.UserID != a1 && *m.UserID != a2) || m.LinkPath != notify.AdminPendingOrgsLink() ||
			m.SourceOrgID == nil || *m.SourceOrgID != co.Org.ID || m.Data["org_id"] != co.Org.ID.String() || len(m.Data) != 1 {
			t.Fatalf("notice %+v", m)
		}
	}
	if len(e.sentOf("orgs.review_requested", notify.ChannelEmail)) != 0 {
		t.Fatal("the review request is in-app only")
	}

	// Decided before the task ran: no notice.
	e2 := reviewEnv(t, a1)
	co2, _ := e2.company(t, "Hızlı Şirket")
	if _, err := e2.svc.AdminVerifyOrg(t.Context(), pad(t), co2.Org.ID, ""); err != nil {
		t.Fatal(err)
	}
	e2.deliver(t, domainevent.TopicOrgReviewRequested)
	if n := len(e2.sentOf("orgs.review_requested", notify.ChannelInApp)); n != 0 {
		t.Fatalf("%d notices for a decided org", n)
	}
}

// TestReview_VerifiedTellsOrgAdmins (BE-144): adminVerifyOrg writes
// org.review_decided (verified); the worker tells the company's active
// owner and admins (not a recruiter) in-app, linking /orgs/{org_id}, and
// by e-mail in their locale with the panel link.
func TestReview_VerifiedTellsOrgAdmins(t *testing.T) {
	t.Parallel()
	e := reviewEnv(t)
	co, owner := e.company(t, "Doğrulanan Şirket")
	admin := e.member(t, co.Org.ID, authz.MemberCompanyAdmin)
	rec := e.member(t, co.Org.ID, authz.MemberRecruiter)
	for _, a := range []authz.Actor{owner, admin, rec} {
		e.users.add(a.UserID, a.UserID.String()+"@sirket.example.test", false)
	}
	if _, err := e.svc.AdminVerifyOrg(t.Context(), pad(t), co.Org.ID, "Belgeler tamam"); err != nil {
		t.Fatal(err)
	}
	evs := eventsOf[domainevent.OrgReviewDecided](t, e, domainevent.TopicOrgReviewDecided)
	if len(evs) != 1 || evs[0].OrgID != co.Org.ID || evs[0].Outcome != domainevent.OrgReviewVerified {
		t.Fatalf("org.review_decided = %+v", evs)
	}
	e.deliver(t, domainevent.TopicOrgReviewDecided)
	want := []uuid.UUID{owner.UserID, admin.UserID}
	inApp, mails := e.sentOf("orgs.org_verified", notify.ChannelInApp), e.sentOf("orgs.org_verified", notify.ChannelEmail)
	if len(inApp) != 2 || len(mails) != 2 {
		t.Fatalf("in-app %d, e-mail %d; want 2 and 2", len(inApp), len(mails))
	}
	for _, m := range inApp {
		if !slices.Contains(want, *m.UserID) || m.LinkPath != notify.OrgLink(co.Org.ID) || m.Data["org_id"] != co.Org.ID.String() {
			t.Fatalf("in-app %+v", m)
		}
	}
	for _, m := range mails {
		if !slices.Contains(want, *m.UserID) || m.ToEmail != m.UserID.String()+"@sirket.example.test" || m.Locale != "en" ||
			m.Data["org_name"] != "Doğrulanan Şirket" || m.Data["name"] != "Ada" ||
			m.Data["link"] != "https://app.koro.test/orgs/"+co.Org.ID.String() {
			t.Fatalf("e-mail %+v", m)
		}
	}
	// Suspending and reactivating are no review decisions.
	n := len(e.out.Events())
	if _, err := e.svc.AdminSuspendOrg(t.Context(), pad(t), co.Org.ID, "Şikâyet incelemesi", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	for _, ev := range e.out.Events()[n:] {
		if ev.Topic == domainevent.TopicOrgReviewDecided || ev.Topic == domainevent.TopicOrgReviewRequested {
			t.Fatalf("suspension wrote %s", ev.Topic)
		}
	}
}

// TestReview_RejectionTellsFounderWithReason (BE-144): platform staff
// closing a pending_review company rejects it: the reason is kept on the
// org (never in the event), org.review_decided (rejected) is written, and
// the worker tells the founder in-app (no link: the membership ended) and
// by e-mail carrying the reason. The owner withdrawing their own pending
// company, and closing an active one, are no rejections.
func TestReview_RejectionTellsFounderWithReason(t *testing.T) {
	t.Parallel()
	e := reviewEnv(t)
	co, owner := e.company(t, "Reddedilen Şirket")
	e.users.add(owner.UserID, "kurucu@sirket.example.test", false)
	const reason = "Vergi levhası doğrulanamadı"
	if _, err := e.svc.CloseOrg(t.Context(), pad(t), co.Org.ID, reason, proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	stored, err := e.store.GetOrg(t.Context(), co.Org.ID)
	if err != nil || stored.Status != orgs.StatusClosed || stored.RejectionReason == nil || *stored.RejectionReason != reason {
		t.Fatalf("closed org = %+v, %v", stored, err)
	}
	evs := eventsOf[domainevent.OrgReviewDecided](t, e, domainevent.TopicOrgReviewDecided)
	if len(evs) != 1 || evs[0].Outcome != domainevent.OrgReviewRejected {
		t.Fatalf("org.review_decided = %+v", evs)
	}
	for _, ev := range e.out.Events() {
		if strings.Contains(string(ev.Payload), "Vergi") {
			t.Fatalf("the reason entered %s", ev.Topic)
		}
	}
	if ev := e.audit.Of(authz.OrgsOrgClose); len(ev) != 1 || ev[0].Metadata["review_outcome"] != "rejected" {
		t.Fatalf("audit %+v", ev)
	}
	e.deliver(t, domainevent.TopicOrgReviewDecided)
	inApp, mails := e.sentOf("orgs.org_rejected", notify.ChannelInApp), e.sentOf("orgs.org_rejected", notify.ChannelEmail)
	if len(inApp) != 1 || *inApp[0].UserID != owner.UserID || !inApp[0].LinkPath.IsZero() || len(inApp[0].Data) != 1 {
		t.Fatalf("in-app %+v", inApp)
	}
	if len(mails) != 1 || mails[0].ToEmail != "kurucu@sirket.example.test" || mails[0].Data["reason"] != reason ||
		mails[0].Data["org_name"] != "Reddedilen Şirket" {
		t.Fatalf("e-mail %+v", mails)
	}

	// The owner withdraws their own pending company: no rejection.
	e2 := reviewEnv(t)
	co2, owner2 := e2.company(t, "Vazgeçen Şirket")
	if _, err := e2.svc.CloseOrg(t.Context(), owner2, co2.Org.ID, "Vazgeçtik", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	// Platform staff close a verified company: no rejection either.
	co3, _ := e2.company(t, "Kapanan Şirket")
	if _, err := e2.svc.AdminVerifyOrg(t.Context(), pad(t), co3.Org.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.svc.CloseOrg(t.Context(), pad(t), co3.Org.ID, "Faaliyet sona erdi", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{co2.Org.ID, co3.Org.ID} {
		if o, _ := e2.store.GetOrg(t.Context(), id); o.RejectionReason != nil {
			t.Fatalf("%s kept a rejection reason", o.Name)
		}
	}
	for _, p := range eventsOf[domainevent.OrgReviewDecided](t, e2, domainevent.TopicOrgReviewDecided) {
		if p.Outcome != domainevent.OrgReviewVerified {
			t.Fatalf("org.review_decided %+v", p)
		}
	}
}

// TestReview_UnknownOutcomeIsPermanent: a malformed decision never retries.
func TestReview_UnknownOutcomeIsPermanent(t *testing.T) {
	t.Parallel()
	e := reviewEnv(t)
	co, _ := e.company(t, "Garip Şirket")
	err := e.publish(t, domainevent.OrgReviewDecided{OrgID: co.Org.ID, OrgKind: "company", Outcome: "maybe"})
	if err == nil || !strings.Contains(err.Error(), "unknown outcome") {
		t.Fatalf("unknown outcome: %v", err)
	}
	// An unknown org is skipped.
	if err := e.publish(t, domainevent.OrgReviewRequested{OrgID: uuid.NewV7(), OrgKind: "company"}); err != nil {
		t.Fatalf("unknown org: %v", err)
	}
}
