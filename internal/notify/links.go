package notify

import (
	"regexp"
	"strings"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
)

// Link is the in-app deep link of an in_app or push notification
// (in_app_notifications.link_path): a client-independent app path that
// web and mobile map to their own routes (BE-122). The zero Link is "no
// link".
//
// A Link has no exported constructor but the route builders below, so
// every link_path a producer can hand to notify is one of the forms of
// the catalog (LinkRoutes, OpenAPI x-koro-link-paths): a new form is a new
// route here, and the contract lists it by construction.
type Link struct{ path string }

// String returns the path ("" for no link).
func (l Link) String() string { return l.path }

// IsZero reports whether the link is empty.
func (l Link) IsZero() bool { return l.path == "" }

// LinkAudience says whose screen a link opens.
type LinkAudience string

// Link audiences.
const (
	LinkForUser  LinkAudience = "user"  // the recipient's own screens (/me/..., an order or invoice of their org)
	LinkForOrg   LinkAudience = "org"   // an organization's management screens (/orgs/{org_id}/..., an event's registrations)
	LinkForAdmin LinkAudience = "admin" // platform staff screens (/admin/...)
)

// LinkRoute is one link_path form of the catalog. Pattern parameters are
// {<name>_id} (a UUID), {club_slug} (an organizations.slug) and
// {order_no} (orders.order_no).
type LinkRoute struct {
	Pattern  string       `json:"pattern"`
	Audience LinkAudience `json:"audience"`
	// Legacy forms are no longer produced; older notifications may still
	// carry them (ParseLink refuses them).
	Legacy bool `json:"legacy,omitempty"`

	re *regexp.Regexp
}

// Parameter expressions of the patterns (the CHECKs of their columns).
const (
	reParamUUID    = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`
	reParamSlug    = `[a-z0-9]+(?:-[a-z0-9]+)*` // organizations_slug_chk
	reParamOrderNo = `KR[0-9A-HJKMNP-TV-Z]{8}`  // orders_order_no_chk
	paramClubSlug  = "{club_slug}"
	paramOrderNo   = "{order_no}"
)

var reParam = regexp.MustCompile(`\{[a-z_]+\}`)

// linkRoutes is the catalog in registration order; route adds to it, so
// a route variable is always listed.
var linkRoutes []*LinkRoute

// route registers a pattern. A malformed or repeated pattern is a
// programming error caught at package initialisation (and by the tests).
func route(pattern string, aud LinkAudience) *LinkRoute {
	var expr strings.Builder
	expr.WriteString("^")
	last := 0
	for _, loc := range reParam.FindAllStringIndex(pattern, -1) {
		expr.WriteString(regexp.QuoteMeta(pattern[last:loc[0]]))
		switch p := pattern[loc[0]:loc[1]]; {
		case p == paramClubSlug:
			expr.WriteString(reParamSlug)
		case p == paramOrderNo:
			expr.WriteString(reParamOrderNo)
		case strings.HasSuffix(p, "_id}"):
			expr.WriteString(reParamUUID)
		default:
			panic("notify: link pattern " + pattern + ": unknown parameter " + p)
		}
		last = loc[1]
	}
	expr.WriteString(regexp.QuoteMeta(pattern[last:]))
	expr.WriteString("$")
	for _, r := range linkRoutes {
		if r.Pattern == pattern {
			panic("notify: link pattern " + pattern + " registered twice")
		}
	}
	r := &LinkRoute{Pattern: pattern, Audience: aud, re: regexp.MustCompile(expr.String())}
	linkRoutes = append(linkRoutes, r)
	return r
}

// legacy registers a form that is no longer produced.
func legacy(pattern string, aud LinkAudience) {
	route(pattern, aud).Legacy = true
}

// link fills the parameters of r in order. Every builder below passes as
// many values as its pattern has parameters (TestLinkBuilders_MatchTheirRoutes).
func (r *LinkRoute) link(values ...string) Link {
	i := 0
	path := reParam.ReplaceAllStringFunc(r.Pattern, func(string) string {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		i++
		return v
	})
	return Link{path: path}
}

// LinkRoutes returns a copy of the catalog: every form a notification's
// link_path may take, legacy forms included.
func LinkRoutes() []LinkRoute {
	out := make([]LinkRoute, len(linkRoutes))
	for i, r := range linkRoutes {
		out[i] = LinkRoute{Pattern: r.Pattern, Audience: r.Audience, Legacy: r.Legacy}
	}
	return out
}

// ParseLink returns the Link of a stored path (outreach_dispatches
// deep_link) when it is a current form of the catalog.
func ParseLink(path string) (Link, bool) {
	for _, r := range linkRoutes {
		if !r.Legacy && r.re.MatchString(path) {
			return Link{path: path}, true
		}
	}
	return Link{}, false
}

// MatchLinkRoute returns the catalog form of path (legacy forms included).
func MatchLinkRoute(path string) (LinkRoute, bool) {
	for _, r := range linkRoutes {
		if r.re.MatchString(path) {
			return LinkRoute{Pattern: r.Pattern, Audience: r.Audience, Legacy: r.Legacy}, true
		}
	}
	return LinkRoute{}, false
}

// The catalog. Producers in brackets.
var (
	// The recipient's own screens.
	routeMyAccessGrants    = route("/me/access-grants", LinkForUser)                         // students
	routeMyClubClaims      = route("/me/club-claims", LinkForUser)                           // directory
	routeMyContactRequest  = route("/me/contact-requests/{contact_request_id}", LinkForUser) // messaging
	routeMyRegistration    = route("/me/registrations/{registration_id}", LinkForUser)       // events
	routeMySecurityEvents  = route("/me/security-events", LinkForUser)                       // notify (audit alerts)
	routeMyTerms           = route("/me/terms", LinkForUser)                                 // identity (a new terms of use version, BE-124)
	routeOrder             = route("/orders/{order_id}", LinkForUser)                        // orders, payments
	routeOrderInstructions = route("/orders/{order_id}/payment-instructions", LinkForUser)   // orders
	routeInvoice           = route("/invoices/{invoice_id}", LinkForUser)                    // billing
	routePost              = route("/posts/{post_id}", LinkForUser)                          // outreach (feed dispatch)
	routeClub              = route("/clubs/by-slug/{club_slug}", LinkForUser)                // directory (claim approved)

	// An organization's management screens.
	routeEventRegistration  = route("/events/{event_id}/registrations/{registration_id}", LinkForOrg) // events
	routeOrgSales           = route("/orgs/{org_id}/sales", LinkForOrg)                               // orders, payments
	routeOrgPayout          = route("/orgs/{org_id}/payouts/{payout_id}", LinkForOrg)                 // ledger
	routeOrgOfferings       = route("/orgs/{org_id}/offerings", LinkForOrg)                           // catalog
	routeOrgOfferingReviews = route("/orgs/{org_id}/offering-approvals", LinkForOrg)                  // catalog
	routeOrgPosts           = route("/orgs/{org_id}/posts", LinkForOrg)                               // feed
	routeOrgPayee           = route("/orgs/{org_id}/payees/{payee_id}", LinkForOrg)                   // parties
	routeOrgContactRequest  = route("/orgs/{org_id}/contact-requests/{contact_request_id}", LinkForOrg)

	// Platform staff screens.
	routeAdminAuditEvents        = route("/admin/audit-events", LinkForAdmin)                        // notify (audit alerts)
	routeAdminBankAccounts       = route("/admin/bank-accounts", LinkForAdmin)                       // parties
	routeAdminBreaches           = route("/admin/breaches", LinkForAdmin)                            // kvkk
	routeAdminClub               = route("/admin/clubs/{club_org_id}", LinkForAdmin)                 // directory
	routeAdminClubClaim          = route("/admin/club-claims/{claim_id}", LinkForAdmin)              // directory
	routeAdminCollectionAccounts = route("/admin/collection-accounts", LinkForAdmin)                 // payments
	routeAdminCommercePolicies   = route("/admin/commerce-policies", LinkForAdmin)                   // catalog
	routeAdminOfferings          = route("/admin/offerings", LinkForAdmin)                           // catalog
	routeAdminOrder              = route("/admin/orders?order_no={order_no}", LinkForAdmin)          // orders
	routeAdminPayout             = route("/admin/payouts/{payout_id}", LinkForAdmin)                 // ledger
	routeAdminRefund             = route("/admin/refunds/{refund_id}", LinkForAdmin)                 // payments
	routeAdminReport             = route("/admin/reports/{report_id}", LinkForAdmin)                 // feed
	routeAdminReviewQueue        = route("/admin/review-queue", LinkForAdmin)                        // feed
	routeAdminUnmatched          = route("/admin/incoming-transfers?status=unmatched", LinkForAdmin) // payments
)

func init() {
	// Before BE-122 the claim approval linked the club by ID; web and
	// mobile resolve it with getClub.
	legacy("/clubs/{club_org_id}", LinkForUser)
}

// Builders of the recipient's own screens.

func MyAccessGrantsLink() Link                { return routeMyAccessGrants.link() }
func MyClubClaimsLink() Link                  { return routeMyClubClaims.link() }
func MyContactRequestLink(req uuid.UUID) Link { return routeMyContactRequest.link(req.String()) }
func MyRegistrationLink(reg uuid.UUID) Link   { return routeMyRegistration.link(reg.String()) }
func MySecurityEventsLink() Link              { return routeMySecurityEvents.link() }
func MyTermsLink() Link                       { return routeMyTerms.link() }
func OrderLink(order uuid.UUID) Link          { return routeOrder.link(order.String()) }
func OrderPaymentInstructionsLink(order uuid.UUID) Link {
	return routeOrderInstructions.link(order.String())
}
func InvoiceLink(invoice uuid.UUID) Link { return routeInvoice.link(invoice.String()) }
func PostLink(post uuid.UUID) Link       { return routePost.link(post.String()) }

// ClubLink is the public page of a club by its slug (organizations.slug
// never changes once set).
func ClubLink(slug string) Link { return routeClub.link(slug) }

// Builders of an organization's management screens.

func EventRegistrationLink(event, reg uuid.UUID) Link {
	return routeEventRegistration.link(event.String(), reg.String())
}
func OrgSalesLink(org uuid.UUID) Link { return routeOrgSales.link(org.String()) }
func OrgPayoutLink(org, payout uuid.UUID) Link {
	return routeOrgPayout.link(org.String(), payout.String())
}
func OrgOfferingsLink(org uuid.UUID) Link         { return routeOrgOfferings.link(org.String()) }
func OrgOfferingApprovalsLink(org uuid.UUID) Link { return routeOrgOfferingReviews.link(org.String()) }
func OrgPostsLink(org uuid.UUID) Link             { return routeOrgPosts.link(org.String()) }
func OrgPayeeLink(org, payee uuid.UUID) Link      { return routeOrgPayee.link(org.String(), payee.String()) }
func OrgContactRequestLink(org, req uuid.UUID) Link {
	return routeOrgContactRequest.link(org.String(), req.String())
}

// Builders of the platform staff screens.

func AdminAuditEventsLink() Link              { return routeAdminAuditEvents.link() }
func AdminBankAccountsLink() Link             { return routeAdminBankAccounts.link() }
func AdminBreachesLink() Link                 { return routeAdminBreaches.link() }
func AdminClubLink(club uuid.UUID) Link       { return routeAdminClub.link(club.String()) }
func AdminClubClaimLink(claim uuid.UUID) Link { return routeAdminClubClaim.link(claim.String()) }
func AdminCollectionAccountsLink() Link       { return routeAdminCollectionAccounts.link() }
func AdminCommercePoliciesLink() Link         { return routeAdminCommercePolicies.link() }
func AdminOfferingsLink() Link                { return routeAdminOfferings.link() }
func AdminOrderLink(orderNo string) Link      { return routeAdminOrder.link(orderNo) }
func AdminPayoutLink(payout uuid.UUID) Link   { return routeAdminPayout.link(payout.String()) }
func AdminRefundLink(refund uuid.UUID) Link   { return routeAdminRefund.link(refund.String()) }
func AdminReportLink(report uuid.UUID) Link   { return routeAdminReport.link(report.String()) }
func AdminReviewQueueLink() Link              { return routeAdminReviewQueue.link() }
func AdminUnmatchedTransfersLink() Link       { return routeAdminUnmatched.link() }

// linkPathText is NotificationDTO.link_path: its OpenAPI description and
// x-koro-link-paths come from the catalog, so the contract can never miss
// a form a producer emits.
type linkPathText string

// ExtLinkPaths is the schema extension listing the link_path forms.
const ExtLinkPaths = "x-koro-link-paths"

// TransformSchema implements huma.SchemaTransformer.
func (linkPathText) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	var b strings.Builder
	b.WriteString("Client-independent in-app path (web and mobile map it to their own routes; a form a client " +
		"does not know is shown without a link). One of:")
	routes := LinkRoutes()
	for i, r := range routes {
		if i > 0 {
			b.WriteString(";")
		}
		b.WriteString(" `" + r.Pattern + "`")
		if r.Legacy {
			b.WriteString(" (legacy)")
		}
	}
	b.WriteString(". {<name>_id} is a UUID, {club_slug} an organization slug (getOrgBySlug), {order_no} an order " +
		"number. Legacy forms are no longer produced but older notifications may carry them.")
	s.Description = b.String()
	if s.Extensions == nil {
		s.Extensions = map[string]any{}
	}
	s.Extensions[ExtLinkPaths] = routes
	return s
}
