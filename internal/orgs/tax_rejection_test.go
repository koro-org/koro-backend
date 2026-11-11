package orgs_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
)

// rejectedCompany is flaggedCompany rejected by a platform admin with reason:
// suspended, the reason kept (BE-208). It returns the view after the
// rejection, the owner and the VKN on file.
func (e *env) rejectedCompany(t *testing.T, name, reason string) (orgs.OrgView, authz.Actor, string) {
	t.Helper()
	v, owner, vkn := e.flaggedCompany(t, name)
	got, err := e.svc.AdminRejectTaxIDChange(t.Context(), pad(t), v.Org.ID, e.adminETag(t, v.Org.ID), reason, proof(goodPassword))
	if err != nil || got.Org.Status != orgs.StatusSuspended {
		t.Fatalf("rejectedCompany: %+v, %v", got.Org, err)
	}
	return got, owner, vkn
}

// TestTaxIDRejection_ReasonShownToEditors (BE-208, Q-63): the rejection keeps
// its time and the staff's reason (trimmed) on the company; the company's
// editors (owner, company_admin) and platform admins read them on the org
// (OrgDTO.company and getOrg's editor view); a recruiter, an outsider, an
// anonymous reader and the admin list never do.
func TestTaxIDRejection_ReasonShownToEditors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	const reason = "VKN şirket unvanıyla eşleşmiyor"
	v, owner, _ := e.rejectedCompany(t, "Gerekçeli Şirket", "  "+reason+"  ")
	id := v.Org.ID
	admin := e.member(t, id, authz.MemberCompanyAdmin)
	rec := e.member(t, id, authz.MemberRecruiter)

	if c := v.Company; c.TaxIDRejectedAt == nil || !c.TaxIDRejectedAt.Equal(t0) || c.TaxIDRejectionReason == nil ||
		*c.TaxIDRejectionReason != reason || c.TaxIDReviewRequestedAt != nil {
		t.Fatalf("company after the rejection = %+v", c)
	}
	if d := orgs.ToOrgDTO(v).Company; d.TaxIDRejectionReason != reason || d.TaxIDRejectedAt == nil || !d.TaxIDRejectedAt.Equal(t0) {
		t.Fatalf("OrgDTO.company = %+v", d)
	}
	for name, a := range map[string]authz.Actor{"owner": owner, "company_admin": admin, "platform_admin": pad(t)} {
		cur, err := e.svc.GetOrg(ctx, a, id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d := orgs.ToPublicDTO(cur).Company
		if d == nil || d.CompanyTaxDTO == nil || d.TaxIDRejectionReason != reason || d.TaxIDRejectedAt == nil {
			t.Fatalf("%s: getOrg company = %+v", name, d)
		}
	}
	for name, a := range map[string]authz.Actor{"recruiter": rec, "outsider": user(), "anonymous": authz.Anonymous(),
		"moderator": persona(t, "moderator")} {
		cur, err := e.svc.GetOrg(ctx, a, id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, err := json.Marshal(orgs.ToPublicDTO(cur))
		if err != nil || strings.Contains(string(body), reason) || strings.Contains(string(body), "tax_id_rejected_at") {
			t.Fatalf("%s reads the rejection: %s, %v", name, body, err)
		}
	}
	page, err := e.svc.AdminListOrgs(ctx, pad(t), orgs.AdminQuery{Kind: orgs.KindCompany})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(orgs.ToAdminDTO(page.Items[0]))
	if err != nil || strings.Contains(string(body), reason) {
		t.Fatalf("the admin list shows the reason: %s, %v", body, err)
	}
	// The audit record keeps the reason too (the staff's trail).
	rows := e.audit.OfOutcome(authz.OrgsOrgSuspend, audit.Success)
	if last := rows[len(rows)-1]; last.Reason != reason {
		t.Fatalf("audit reason = %q", last.Reason)
	}
}

// TestTaxIDRejection_ClearedWithTheSuspension (BE-208): the reason lives as
// long as the suspension it explains. A platform admin's correction to
// another identifier clears it (the same VKN again does not), so does the
// reactivation and the closing; an active company never shows one.
func TestTaxIDRejection_ClearedWithTheSuspension(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cleared := func(t *testing.T, id uuid.UUID) {
		t.Helper()
		c, err := e.store.GetCompany(ctx, id)
		if err != nil || c.TaxIDRejectedAt != nil || c.TaxIDRejectionReason != nil {
			t.Fatalf("rejection kept: %+v, %v", c, err)
		}
	}

	// The correction: the same VKN keeps the reason, another one clears it.
	v, _, vkn := e.rejectedCompany(t, "Düzeltilen Şirket", "Yanlış VKN girilmiş")
	id := v.Org.ID
	same, err := e.svc.UpdateOrg(ctx, pad(t), id, e.adminETag(t, id), orgs.OrgPatch{TaxNo: &vkn})
	if err != nil || same.Company.TaxIDRejectionReason == nil {
		t.Fatalf("the same VKN = %+v, %v", same.Company, err)
	}
	fixed := testVKN
	cor, err := e.svc.UpdateOrg(ctx, pad(t), id, e.adminETag(t, id), orgs.OrgPatch{TaxNo: &fixed, Proof: proof(goodPassword)})
	if err != nil || orgs.ToOrgDTO(cor).Company.TaxIDRejectionReason != "" {
		t.Fatalf("corrected = %+v, %v", cor.Company, err)
	}
	cleared(t, id)

	// The reactivation.
	v2, _, _ := e.rejectedCompany(t, "Geri Açılan Şirket", "Vergi levhası okunamadı")
	if _, err := e.svc.AdminReactivateOrg(ctx, pad(t), v2.Org.ID, "Vergi levhası yeniden gönderildi", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	cleared(t, v2.Org.ID)
	cur, err := e.svc.GetOrg(ctx, pad(t), v2.Org.ID)
	if err != nil || orgs.ToPublicDTO(cur).Company.TaxIDRejectionReason != "" {
		t.Fatalf("reactivated view = %+v, %v", cur.Company, err)
	}
	// Another review after the reactivation is a fresh one (no rejection kept).
	owner2 := e.member(t, v2.Org.ID, authz.MemberCompanyAdmin)
	other := testVKN // the company holds otherVKN since flaggedCompany
	next, err := e.svc.UpdateOrg(ctx, owner2, v2.Org.ID, e.adminETag(t, v2.Org.ID), orgs.OrgPatch{TaxNo: &other,
		Proof: proof(goodPassword)})
	if err != nil || next.Company.TaxIDReviewRequestedAt == nil || next.Company.TaxIDRejectedAt != nil {
		t.Fatalf("a new review after the reactivation = %+v, %v", next.Company, err)
	}

	// The closing.
	v3, _, _ := e.rejectedCompany(t, "Kapanan Reddedilmiş", "VKN başka bir şirkete ait")
	if _, err := e.svc.CloseOrg(ctx, pad(t), v3.Org.ID, "Şirket faaliyetini bitirdi", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	cleared(t, v3.Org.ID)

	// A suspension for another reason, lifted, leaves nothing behind either.
	v4, _ := e.company(t, "Sade Askı")
	if _, err := e.svc.AdminVerifyOrg(ctx, pad(t), v4.Org.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminSuspendOrg(ctx, pad(t), v4.Org.ID, "Şikâyet incelemesi", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminReactivateOrg(ctx, pad(t), v4.Org.ID, "İnceleme bitti", proof(goodPassword)); err != nil {
		t.Fatal(err)
	}
	cleared(t, v4.Org.ID)
}

// TestTaxIDReview_PartyCannotDecide (BE-208, Q-63): a platform admin who is a
// member of the company (any role) neither approves nor rejects its new tax
// identifier: 403 forbidden before the review, If-Match and the proof (no
// proof is checked), recorded as denied (reason org_member, the org's scope,
// no number); the review keeps waiting and another platform admin decides. A
// membership in another company changes nothing.
func TestTaxIDReview_PartyCannotDecide(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v, _, vkn := e.flaggedCompany(t, "Taraf Şirketi")
	id := v.Org.ID
	etag := e.adminETag(t, id)
	other, _ := e.company(t, "Başka Şirket")

	for _, role := range []authz.MemberRole{authz.MemberRecruiter, authz.MemberCompanyAdmin, authz.MemberCompanyOwner} {
		party := as(pad(t), id, orgs.KindCompany, role)
		calls := e.reauth.calls
		_, err := e.svc.AdminApproveTaxIDChange(ctx, party, id, etag, "")
		problemtest.WantErr(t, err, authz.ErrForbidden)
		_, err = e.svc.AdminRejectTaxIDChange(ctx, party, id, etag, "Kendi şirketim", proof(goodPassword))
		problemtest.WantErr(t, err, authz.ErrForbidden)
		// The party rule comes before the review and If-Match: a stale ETag is 403 too.
		_, err = e.svc.AdminApproveTaxIDChange(ctx, party, id, `"0"`, "")
		problemtest.WantErr(t, err, authz.ErrForbidden)
		if e.reauth.calls != calls {
			t.Fatalf("%s: a proof was checked for a party", role)
		}
	}
	for _, act := range []authz.Action{authz.OrgsOrgVerify, authz.OrgsOrgSuspend} {
		denied := e.audit.OfOutcome(act, audit.Denied)
		if len(denied) == 0 {
			t.Fatalf("%s: no denied record", act)
		}
		for _, d := range denied {
			if d.Reason != "org_member" || d.OrgID == nil || *d.OrgID != id || d.ResourceID == nil || *d.ResourceID != id ||
				d.Metadata["tax_id_review"] == nil || strings.Contains(d.Reason+string(mustJSON(t, d.Metadata)), vkn) {
				t.Fatalf("%s denied record = %+v", act, d)
			}
		}
	}
	if n := len(e.audit.OfOutcome(authz.OrgsOrgVerify, audit.Success)); n != 1 { // the registration's verification only
		t.Fatalf("%d successful orgs.org.verify records", n)
	}
	cur, err := e.store.GetCompany(ctx, id)
	if err != nil || cur.TaxIDReviewRequestedAt == nil {
		t.Fatalf("the review ended: %+v, %v", cur, err)
	}

	// A platform admin of another company, and one of none, decide.
	elsewhere := as(pad(t), other.Org.ID, orgs.KindCompany, authz.MemberCompanyAdmin)
	got, err := e.svc.AdminApproveTaxIDChange(ctx, elsewhere, id, etag, "")
	if err != nil || got.Company.TaxIDReviewRequestedAt != nil {
		t.Fatalf("approval by a platform admin of another company = %+v, %v", got.Company, err)
	}
	v2, _, _ := e.flaggedCompany(t, "Taraf Şirketi İki")
	party := as(pad(t), v2.Org.ID, orgs.KindCompany, authz.MemberCompanyAdmin)
	_, err = e.svc.AdminRejectTaxIDChange(ctx, party, v2.Org.ID, e.adminETag(t, v2.Org.ID), "Kendi şirketim", proof(goodPassword))
	problemtest.WantErr(t, err, authz.ErrForbidden)
	if _, err := e.svc.AdminRejectTaxIDChange(ctx, pad(t), v2.Org.ID, e.adminETag(t, v2.Org.ID), "Yanlış VKN",
		proof(goodPassword)); err != nil {
		t.Fatalf("rejection by a platform admin who is no party: %v", err)
	}
}

// TestTaxIDReview_PartyStillSeesTheQueue (BE-208): the party rule decides,
// it does not hide. A platform admin who is a member of the company still
// finds it in the queue (adminListOrgs?tax_id_review, the shared counter)
// and gets the review notice like every platform admin (BE-194's choice for
// the report notices): nothing in either is secret from a member.
func TestTaxIDReview_PartyStillSeesTheQueue(t *testing.T) {
	t.Parallel()
	partyID := persona(t, "platform_admin").UserID
	e := reviewEnv(t, partyID)
	ctx := t.Context()
	v, _, _ := e.flaggedCompany(t, "Görünen Taraf")
	party := as(pad(t), v.Org.ID, orgs.KindCompany, authz.MemberCompanyAdmin)
	page, err := e.svc.AdminListOrgs(ctx, party, orgs.AdminQuery{TaxIDReview: true})
	if err != nil || len(page.Items) != 1 || page.Items[0].Org.ID != v.Org.ID {
		t.Fatalf("the party's queue = %+v, %v", page.Items, err)
	}
	e.deliver(t, domainevent.TopicOrgReviewRequested)
	got := e.sentOf("orgs.tax_id_review_requested", notify.ChannelInApp)
	if len(got) != 1 || *got[0].UserID != partyID {
		t.Fatalf("review notices = %+v", got)
	}
}

// TestTaxIDRejected_TemplateCarriesTheReason (BE-208): the rejection e-mail
// shows the reason (escaped in HTML) in both locales and reads well without
// one (a rejection from before BE-208); the in-app text is fixed: no reason.
func TestTaxIDRejected_TemplateCarriesTheReason(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	const reason = "VKN <b>unvanla</b> eşleşmiyor"
	for _, loc := range []string{"tr", "en"} {
		r, err := tpl.Render("orgs.tax_id_rejected", loc, notify.RenderInput{Data: map[string]any{
			"name": "Ayşe", "org_name": "Örnek A.Ş.", "reason": reason}})
		if err != nil {
			t.Fatalf("%s: %v", loc, err)
		}
		if r.Version != 2 || !strings.Contains(r.Text, reason) || !strings.Contains(r.HTML, "VKN &lt;b&gt;unvanla&lt;/b&gt; eşleşmiyor") ||
			strings.Contains(r.HTML, "<b>unvanla") || strings.Contains(r.Subject, "unvanla") {
			t.Fatalf("%s: rendered %+v", loc, r)
		}
		none, err := tpl.Render("orgs.tax_id_rejected", loc, notify.RenderInput{Data: map[string]any{
			"name": "Ayşe", "org_name": "Örnek A.Ş.", "reason": ""}})
		if err != nil || slices.ContainsFunc([]string{"Gerekçe:", "Reason:"}, func(s string) bool {
			return strings.Contains(none.Text, s) || strings.Contains(none.HTML, s)
		}) {
			t.Fatalf("%s without a reason: %+v, %v", loc, none, err)
		}
		n, ok := tpl.Notice("orgs.tax_id_rejected", loc)
		if !ok || strings.Contains(n.Body, "{{") || n.Body == "" {
			t.Fatalf("%s notice = %+v, %v", loc, n, ok)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
