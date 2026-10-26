package notify

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
)

// builderCases is every Link builder with the pattern it must produce.
func builderCases() map[string]struct {
	got     Link
	pattern string
} {
	a, b, d := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	type c = struct {
		got     Link
		pattern string
	}
	return map[string]c{
		"MyAccessGrantsLink":           {MyAccessGrantsLink(), "/me/access-grants"},
		"MyClubClaimsLink":             {MyClubClaimsLink(), "/me/club-claims"},
		"MyContactRequestLink":         {MyContactRequestLink(a), "/me/contact-requests/{contact_request_id}"},
		"MyRegistrationLink":           {MyRegistrationLink(a), "/me/registrations/{registration_id}"},
		"MySecurityEventsLink":         {MySecurityEventsLink(), "/me/security-events"},
		"MyTermsLink":                  {MyTermsLink(), "/me/terms"},
		"MyStudentVerificationLink":    {MyStudentVerificationLink(), "/me/student-verification"},
		"OrderLink":                    {OrderLink(a), "/orders/{order_id}"},
		"OrderPaymentInstructionsLink": {OrderPaymentInstructionsLink(a), "/orders/{order_id}/payment-instructions"},
		"InvoiceLink":                  {InvoiceLink(a), "/invoices/{invoice_id}"},
		"PostLink":                     {PostLink(a), "/posts/{post_id}"},
		"ClubLink":                     {ClubLink("bilisim-kulubu-2"), "/clubs/by-slug/{club_slug}"},
		"EventRegistrationLink":        {EventRegistrationLink(a, b, d), "/orgs/{org_id}/events/{event_id}/registrations/{registration_id}"},
		"OrgSalesLink":                 {OrgSalesLink(a), "/orgs/{org_id}/sales"},
		"OrgPayoutLink":                {OrgPayoutLink(a, b), "/orgs/{org_id}/payouts/{payout_id}"},
		"OrgOfferingsLink":             {OrgOfferingsLink(a), "/orgs/{org_id}/offerings"},
		"OrgOfferingApprovalsLink":     {OrgOfferingApprovalsLink(a), "/orgs/{org_id}/offering-approvals"},
		"OrgPostsLink":                 {OrgPostsLink(a), "/orgs/{org_id}/posts"},
		"OrgPayeeLink":                 {OrgPayeeLink(a, b), "/orgs/{org_id}/payees/{payee_id}"},
		"OrgContactRequestLink":        {OrgContactRequestLink(a, b), "/orgs/{org_id}/contact-requests/{contact_request_id}"},
		"AdminAuditEventsLink":         {AdminAuditEventsLink(), "/admin/audit-events"},
		"AdminBankAccountsLink":        {AdminBankAccountsLink(), "/admin/bank-accounts"},
		"AdminBreachesLink":            {AdminBreachesLink(), "/admin/breaches"},
		"AdminClubLink":                {AdminClubLink(a), "/admin/clubs/{club_org_id}"},
		"AdminClubClaimLink":           {AdminClubClaimLink(a), "/admin/club-claims/{claim_id}"},
		"AdminCollectionAccountsLink":  {AdminCollectionAccountsLink(), "/admin/collection-accounts"},
		"AdminCommercePoliciesLink":    {AdminCommercePoliciesLink(), "/admin/commerce-policies"},
		"AdminOfferingsLink":           {AdminOfferingsLink(), "/admin/offerings"},
		"AdminOrderLink":               {AdminOrderLink("KR7K3M9Q2X"), "/admin/orders?order_no={order_no}"},
		"AdminPlatformRolesLink":       {AdminPlatformRolesLink(), "/admin/platform-roles"},
		"AdminPayoutLink":              {AdminPayoutLink(a), "/admin/payouts/{payout_id}"},
		"AdminRefundLink":              {AdminRefundLink(a), "/admin/refunds/{refund_id}"},
		"AdminReportLink":              {AdminReportLink(a), "/admin/reports/{report_id}"},
		"AdminReviewQueueLink":         {AdminReviewQueueLink(), "/admin/review-queue"},
		"AdminUnmatchedTransfersLink":  {AdminUnmatchedTransfersLink(), "/admin/incoming-transfers?status=unmatched"},
	}
}

// TestLinkBuilders_MatchTheirRoutes: every builder fills its catalog form
// completely (no placeholder left), the path passes the link_path CHECK,
// ParseLink accepts it and resolves it to that form; every current form
// has a builder.
func TestLinkBuilders_MatchTheirRoutes(t *testing.T) {
	t.Parallel()
	built := map[string]bool{}
	for name, c := range builderCases() {
		p := c.got.String()
		r, ok := MatchLinkRoute(p)
		switch {
		case strings.ContainsAny(p, "{}") || !reLinkPath.MatchString(p) || len(p) > maxLinkPath:
			t.Errorf("%s() = %q: not a valid link_path", name, p)
		case !ok || r.Pattern != c.pattern || r.Legacy:
			t.Errorf("%s() = %q: matches %+v, want %s", name, p, r, c.pattern)
		}
		if l, ok := ParseLink(p); !ok || l != c.got {
			t.Errorf("ParseLink(%q) = %v, %v", p, l, ok)
		}
		built[c.pattern] = true
	}
	for _, r := range LinkRoutes() {
		if !r.Legacy && !built[r.Pattern] {
			t.Errorf("route %s has no builder case", r.Pattern)
		}
	}
	if (Link{}).String() != "" || !(Link{}).IsZero() {
		t.Fatal("the zero Link is not empty")
	}
}

// TestLinkCatalog_Unambiguous: patterns are unique, and no path of one
// form is read as another (the legacy /clubs/{club_org_id} and
// /clubs/by-slug/{club_slug} included); legacy forms are not parsed as
// current links; foreign paths are refused.
func TestLinkCatalog_Unambiguous(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, r := range LinkRoutes() {
		if seen[r.Pattern] || !strings.HasPrefix(r.Pattern, "/") || strings.HasPrefix(r.Pattern, "//") {
			t.Errorf("pattern %q repeated or not an app path", r.Pattern)
		}
		seen[r.Pattern] = true
		switch r.Audience {
		case LinkForUser, LinkForOrg, LinkForAdmin:
		default:
			t.Errorf("pattern %q: audience %q", r.Pattern, r.Audience)
		}
	}
	for _, c := range builderCases() {
		n := 0
		for _, r := range linkRoutes {
			if r.re.MatchString(c.got.String()) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%q matches %d forms", c.got, n)
		}
	}
	legacy := "/clubs/" + uuid.NewV7().String()
	if r, ok := MatchLinkRoute(legacy); !ok || !r.Legacy {
		t.Errorf("legacy club link = %+v, %v", r, ok)
	}
	for _, p := range []string{legacy, "/clubs/by-slug/A_B", "/admin/orders?order_no=KR1&x=//e", "/orders/not-a-uuid",
		"//evil.example/x", "/me/access-grants/x", "/posts/" + uuid.NewV7().String() + "?x=1", ""} {
		if l, ok := ParseLink(p); ok {
			t.Errorf("ParseLink(%q) = %v, want refused", p, l)
		}
	}
}

// TestNotificationDTO_LinkPathContract: the OpenAPI schema of link_path
// lists every catalog form, in the description and in x-koro-link-paths.
func TestNotificationDTO_LinkPathContract(t *testing.T) {
	t.Parallel()
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	s := huma.SchemaFromType(reg, reflect.TypeFor[NotificationDTO]())
	if s.Ref != "" {
		s = reg.SchemaFromRef(s.Ref)
	}
	lp := s.Properties["link_path"]
	if lp == nil {
		t.Fatal("no link_path property")
	}
	routes := LinkRoutes()
	for _, r := range routes {
		if !strings.Contains(lp.Description, "`"+r.Pattern+"`") {
			t.Errorf("description lacks %s", r.Pattern)
		}
	}
	got, ok := lp.Extensions[ExtLinkPaths].([]LinkRoute)
	if !ok || !slices.EqualFunc(got, routes, func(a, b LinkRoute) bool {
		return a.Pattern == b.Pattern && a.Audience == b.Audience && a.Legacy == b.Legacy
	}) {
		t.Fatalf("%s = %#v", ExtLinkPaths, lp.Extensions[ExtLinkPaths])
	}
}

// TestLinkProducers_AllInCatalog: every producer outside notify builds its
// link_path with a catalog builder (a Link has no other constructor), and
// every builder a producer calls is one of builderCases; every builder is
// used by some producer (no dead form). The source scan keeps the catalog
// and its consumers in step when a producer is added or removed.
func TestLinkProducers_AllInCatalog(t *testing.T) {
	t.Parallel()
	reCall := regexp.MustCompile(`\bnotify\.([A-Z][A-Za-z]*Link)\(`)
	reLocal := regexp.MustCompile(`\b((?:My|Admin)[A-Za-z]*Link)\(\)`)
	cases := builderCases()
	used := map[string]bool{}
	root := filepath.Join("..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		names := reCall.FindAllSubmatch(src, -1)
		if filepath.Dir(path) == "." || filepath.Base(filepath.Dir(path)) == "notify" {
			if filepath.Base(path) == "links.go" {
				return nil
			}
			names = reLocal.FindAllSubmatch(src, -1)
		}
		for _, m := range names {
			name := string(m[1])
			if _, ok := cases[name]; !ok && name != "ParseLink" {
				t.Errorf("%s: %s is not a catalog builder", path, name)
			}
			used[name] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range cases {
		if !used[name] {
			t.Errorf("builder %s has no producer", name)
		}
	}
}

// TestLinkRoutes_OrgFormsNameTheOrg (BE-127): every current org-audience
// form starts with /orgs/{org_id}/, so a client opens it in the panel of
// that org's kind; the old registration form is legacy only.
func TestLinkRoutes_OrgFormsNameTheOrg(t *testing.T) {
	t.Parallel()
	for _, r := range LinkRoutes() {
		if r.Audience == LinkForOrg && !r.Legacy && !strings.HasPrefix(r.Pattern, "/orgs/{org_id}/") {
			t.Errorf("org link %s does not name its org", r.Pattern)
		}
	}
	old := "/events/" + uuid.NewV7().String() + "/registrations/" + uuid.NewV7().String()
	if r, ok := MatchLinkRoute(old); !ok || !r.Legacy {
		t.Fatalf("old registration form = %+v, %v; want legacy", r, ok)
	}
	if _, ok := ParseLink(old); ok {
		t.Fatal("ParseLink accepts the legacy registration form")
	}
}
