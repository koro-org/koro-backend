package seed

import (
	"time"

	"koro/internal/notify"
)

// Messaging and outreach texts of the demo (synthetic, no personal data).
const (
	demoContactSubject = "Yaz stajı görüşmesi"
	demoContactMessage = "Merhaba, profilinizi keşif ekranında gördük. Yaz stajı programımız hakkında konuşmak isteriz."
	demoCompanyReply   = "Kabul ettiğiniz için teşekkürler. Uygun olduğunuz bir gün kısa bir görüşme yapabilir miyiz?"
	demoStudentReply   = "Merhaba, memnuniyetle. Önümüzdeki hafta salı öğleden sonra uygunum."
)

// companyActivity (BE-179) gives the main company the child records a
// contract fuzzer needs to reach real 200 responses under /orgs/{org_id}:
// a contact_request and a targeted_dispatch grant (admin grants, like the
// targeted_post one), an accepted contact request to StudentEmail with the
// open conversation its acceptance started (one message each side), and
// the completed push dispatch of the company's targeted post. Each spends
// one unit of its grant through a usage row, as the services do. It draws
// nothing from the random source, so the rest of the demo is unchanged.
func (b *builder) companyActivity() {
	c := b.companies[0]
	s := b.students[0] // StudentEmail: adult, main university
	from, to := b.now.AddDate(0, 0, -20), b.now.AddDate(0, 6, 0)
	grant := func(n int, feature string) {
		b.p.add("entitlements", "id", b.id("ent", n), "org_id", c.orgID, "feature", feature, "kind", "quota",
			"quantity_total", 5, "quantity_used", 1, "valid_from", from, "valid_to", to, "status", "active",
			"source_type", "admin_grant", "granted_by", b.admin1, "activated_at", from, "created_at", from, "updated_at", from)
	}
	const contactEnt, dispatchEnt = 100, 101 // beyond the per-company targeted_post grants
	grant(contactEnt, "contact_request")
	grant(dispatchEnt, "targeted_dispatch")

	req, conv := b.id("contact", 0), b.id("conversation", 0)
	sent := b.now.AddDate(0, 0, -10)
	accepted := sent.Add(20 * time.Hour)
	usage := b.id("usage", 1)
	b.p.add("entitlement_usages", "id", usage, "entitlement_id", b.id("ent", contactEnt), "org_id", c.orgID, "quantity", 1,
		"ref_type", "contact_request", "ref_id", req, "actor_user_id", c.owner, "created_at", sent)
	b.p.add("contact_requests", "id", req, "org_id", c.orgID, "org_kind", "company", "sender_user_id", c.owner,
		"student_user_id", s.id, "via", "discovery", "subject", demoContactSubject, "message", demoContactMessage,
		"status", "accepted", "responded_at", accepted, "expires_at", sent.AddDate(0, 0, 14),
		"entitlement_usage_id", usage, "created_at", sent, "updated_at", accepted)
	companyAt, studentAt := accepted.Add(2*time.Hour), accepted.Add(26*time.Hour)
	b.p.add("conversations", "id", conv, "contact_request_id", req, "org_id", c.orgID, "org_kind", "company",
		"student_user_id", s.id, "company_user_id", c.owner, "status", "open", "company_unanswered", 0,
		"last_activity_at", studentAt, "created_at", accepted, "updated_at", studentAt)
	b.p.add("conversation_messages", "id", b.id("message", 0), "conversation_id", conv, "sender_side", "company",
		"sender_user_id", c.owner, "body", demoCompanyReply, "created_at", companyAt)
	b.p.add("conversation_messages", "id", b.id("message", 1), "conversation_id", conv, "sender_side", "student",
		"sender_user_id", s.id, "body", demoStudentReply, "created_at", studentAt)

	// The dispatch of the targeted post (targetedPost: published three days
	// ago to the main university); commercial, so push only (outreach
	// checkChannels), counted when it completed.
	dispatch := b.id("dispatch", 0)
	started := b.now.AddDate(0, 0, -3).Add(5 * time.Minute)
	dusage := b.id("usage", 2)
	eligible := b.eligibleIn(0)
	consented := eligible / 2
	b.p.add("entitlement_usages", "id", dusage, "entitlement_id", b.id("ent", dispatchEnt), "org_id", c.orgID, "quantity", 1,
		"ref_type", "dispatch", "ref_id", dispatch, "actor_user_id", c.owner, "created_at", started.Add(-time.Minute))
	u := b.ref.Universities[0]
	b.p.add("outreach_dispatches", "id", dispatch, "org_id", c.orgID, "post_id", c.targeted, "segment_id", c.segment,
		"audience_keys", []string{"uni:" + u.ID.String()}, "audience_dim_count", 1, "channels", []string{"push"},
		"title_snapshot", targetedPostTitle, "deep_link", notify.PostLink(c.targeted).String(), "commercial", true,
		"status", "completed", "eligible_count", eligible, "consented_count", consented, "sent_count", consented,
		"entitlement_usage_id", dusage, "started_at", started, "completed_at", started.Add(2*time.Minute),
		"created_by", c.owner, "created_at", started.Add(-time.Minute), "updated_at", started.Add(2*time.Minute))

	b.p.Demo.ContactRequestID, b.p.Demo.ConversationID, b.p.Demo.DispatchID = req, conv, dispatch
}
