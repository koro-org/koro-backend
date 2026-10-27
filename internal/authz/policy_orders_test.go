package authz_test

import (
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// orderOf is the Resource the orders service builds (BE-040): OrgID = the
// buyer; Attrs status and the comma-separated seller orgs of its lines.
func orderOf(buyer string, sellers ...string) authz.Resource {
	id := authztest.ID(buyer)
	ids := make([]string, len(sellers))
	for i, s := range sellers {
		ids[i] = authztest.ID(s).String()
	}
	return authz.Resource{Type: authz.TypeOrder, ID: uuid.NewV7(), OrgID: &id, Attrs: map[string]string{
		authz.AttrStatus: "awaiting_payment", authz.AttrOrderSellerOrgIDs: strings.Join(ids, ","),
	}}
}

// lineOf is an order line of the buyer sold by seller.
func lineOf(buyer, seller string) authz.Resource {
	id := authztest.ID(buyer)
	return authz.Resource{Type: authz.TypeOrderLine, ID: uuid.NewV7(), OrgID: &id, Attrs: map[string]string{
		authz.AttrStatus: "pending", authz.AttrOrderLineSellerOrgID: authztest.ID(seller).String(),
	}}
}

// TestOrdersPolicy_Matrix pins 03 §5.15 for the 18 personas (the HTTP
// matrix, testdata/authz/orders.yaml, checks the same through the API).
func TestOrdersPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	// surface: anonymous 403 (no session reaches the service: 401 at the API),
	// members of the listed orgs and every platform role 403, everyone else
	// 404; allow overrides.
	surface := func(members []string, allow map[string]error) map[string]error {
		m := map[string]error{"anonymous": forbidden}
		for _, p := range authztest.Personas() {
			if p.Name == "anonymous" {
				continue
			}
			m[p.Name] = notFound
			if p.Actor.HasAnyPlatformRole() {
				m[p.Name] = forbidden
			}
		}
		for _, n := range members {
			m[n] = forbidden
		}
		for k, v := range allow {
			m[k] = v
		}
		return m
	}
	companyA := []string{"company_member", "company_admin"}
	clubA := []string{"club_member", "club_admin"}
	both := append(append([]string{}, companyA...), clubA...)
	order := orderOf("company_a", "club_a")
	type cell struct {
		name string
		act  authz.Action
		r    authz.Resource
		want map[string]error
	}
	cells := []cell{
		{"create (k25)", authz.OrdersOrderCreate, orgOf("company_a", "company"), surface(companyA, map[string]error{"company_admin": nil})},
		{"create for another company (IDOR)", authz.OrdersOrderCreate, orgOf("company_b", "company"),
			surface([]string{"other_company_member"}, map[string]error{"other_company_member": nil})},
		{"quote (BE-136: the cell of create)", authz.OrdersOrderQuote, orgOf("company_a", "company"),
			surface(companyA, map[string]error{"company_admin": nil})},
		{"quote for another company (IDOR)", authz.OrdersOrderQuote, orgOf("company_b", "company"),
			surface([]string{"other_company_member"}, map[string]error{"other_company_member": nil})},
		{"list a company's orders", authz.OrdersOrderList, orgOf("company_a", "company"),
			surface(companyA, map[string]error{"company_member": nil, "company_admin": nil})},
		{"read an order (k26: the seller's club_admin)", authz.OrdersOrderRead, order, surface(both, map[string]error{
			"company_member": nil, "company_admin": nil, "club_admin": nil, "support": nil, "platform_admin": nil, "finance_admin": nil})},
		{"read another company's order", authz.OrdersOrderRead, orderOf("company_b", "club_b"), surface([]string{"other_company_member", "other_club_admin"},
			map[string]error{"other_company_member": nil, "other_club_admin": nil, "support": nil, "platform_admin": nil, "finance_admin": nil})},
		{"payment instructions", authz.OrdersOrderPaymentInstructions, order,
			surface(both, map[string]error{"company_member": nil, "company_admin": nil, "finance_admin": nil})},
		{"cancel", authz.OrdersOrderCancel, order,
			surface(both, map[string]error{"company_admin": nil, "platform_admin": nil, "finance_admin": nil})},
		{"sales of a club", authz.OrdersLineListSales, orgOf("club_a", "club"),
			surface(clubA, map[string]error{"club_admin": nil, "platform_admin": nil, "finance_admin": nil})},
		{"sales of a university", authz.OrdersLineListSales, orgOf("uni_a", "university"),
			surface([]string{"university_rep"}, map[string]error{"university_rep": nil, "platform_admin": nil, "finance_admin": nil})},
		{"mark a line delivered", authz.OrdersLineMarkFulfilled, lineOf("company_a", "club_a"),
			surface(both, map[string]error{"club_admin": nil, "platform_admin": nil})},
		{"mark a university's line delivered", authz.OrdersLineMarkFulfilled, lineOf("company_a", "uni_a"),
			surface(append([]string{"university_rep"}, companyA...), map[string]error{"university_rep": nil, "platform_admin": nil})},
		{"read the delivery evidence (BE-093)", authz.OrdersLineReadEvidence, lineOf("company_a", "club_a"),
			surface(both, map[string]error{"company_admin": nil, "club_admin": nil, "platform_admin": nil, "finance_admin": nil})},
		{"read a university's delivery evidence", authz.OrdersLineReadEvidence, lineOf("company_a", "uni_a"),
			surface(append([]string{"university_rep"}, companyA...),
				map[string]error{"company_admin": nil, "university_rep": nil, "platform_admin": nil, "finance_admin": nil})},
		{"read another company's delivery evidence", authz.OrdersLineReadEvidence, lineOf("company_b", "club_a"),
			surface(append([]string{"other_company_member"}, clubA...),
				map[string]error{"other_company_member": nil, "club_admin": nil, "platform_admin": nil, "finance_admin": nil})},
		{"confirm a line", authz.OrdersLineConfirm, lineOf("company_a", "club_a"), surface(both, map[string]error{"company_admin": nil})},
		{"dispute a line", authz.OrdersLineDispute, lineOf("company_a", "club_a"), surface(both, map[string]error{"company_admin": nil})},
		{"resolve a dispute (after the gate)", authz.OrdersLineResolveDispute, lineOf("company_a", "club_a"),
			surface(both, map[string]error{"platform_admin": nil, "finance_admin": nil})},
	}
	for _, c := range cells {
		for _, p := range authztest.Personas() {
			want, ok := c.want[p.Name]
			if !ok {
				t.Fatalf("%s: no expectation for %s", c.name, p.Name)
			}
			err := authz.Can(ctx, p.Actor, c.act, c.r)
			if want == nil && err != nil || want != nil && err == nil {
				t.Errorf("%s: %s: %v, want %v", c.name, p.Name, err, want)
				continue
			}
			if want != nil {
				wantErr(t, err, want, false)
			}
		}
	}
	// Admin surface: 403 before reading for everyone without the role.
	for act, allowed := range map[authz.Action][]string{
		authz.OrdersOrderListAdmin:     {"support", "platform_admin", "finance_admin"},
		authz.OrdersLineResolveDispute: {"platform_admin", "finance_admin"},
	} {
		for _, p := range authztest.Personas() {
			var want error = forbidden
			for _, n := range allowed {
				if p.Name == n {
					want = nil
				}
			}
			wantErr(t, authz.Can(ctx, p.Actor, act, adminSurface), want, false)
		}
	}
}

// TestOrdersPolicy_EdgeCases: MFA of the company admin and staff paths
// (k-mfa), the owner, career_staff on k26 and the university's k-cs, the
// system-only actions, missing Attrs.
func TestOrdersPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company := orgOf("company_a", "company")
	cma := persona(t, "company_admin")
	cma.MFAVerified = false
	wantErr(t, authz.Can(ctx, cma, authz.OrdersOrderCreate, company), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, cma, authz.OrdersOrderQuote, company), authz.ErrMFARequired, false) // BE-136: k-mfa as create
	cmm := persona(t, "company_member")
	cmm.MFAVerified = false // recruiters need no MFA
	wantErr(t, authz.Can(ctx, cmm, authz.OrdersOrderList, company), nil, false)
	fin := persona(t, "finance_admin")
	fin.MFAVerified = false
	wantErr(t, authz.Can(ctx, fin, authz.OrdersOrderListAdmin, adminSurface), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, persona(t, "company_owner_a"), authz.OrdersOrderCreate, company), nil, false)
	// A company admin is no buyer on an org that is not a company.
	wantErr(t, authz.Can(ctx, persona(t, "company_admin"), authz.OrdersOrderCreate, orgOf("company_a", "club")), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, persona(t, "company_owner_a"), authz.OrdersOrderQuote, company), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "company_admin"), authz.OrdersOrderQuote, orgOf("company_a", "club")), authz.ErrForbidden, false)
	// A recruiter neither buys nor previews the price of a purchase (BE-136).
	wantErr(t, authz.Can(ctx, cmm, authz.OrdersOrderQuote, company), authz.ErrForbidden, false)

	// k26 (no k-cs): career_staff of a seller university reads the order; k-cs:
	// it does not mark a line delivered.
	cs := persona(t, "university_rep")
	cs.Memberships = []authz.Membership{{OrgID: authztest.ID("uni_a"), OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}
	cs.MFAVerified = false
	wantErr(t, authz.Can(ctx, cs, authz.OrdersOrderRead, orderOf("company_a", "uni_a")), nil, false)
	wantErr(t, authz.Can(ctx, cs, authz.OrdersLineMarkFulfilled, lineOf("company_a", "uni_a")), authz.ErrForbidden, false)
	// BE-093: nor does it read the delivery evidence; the company admin needs an MFA
	// session for it (k-mfa), the seller's club admin none.
	wantErr(t, authz.Can(ctx, cs, authz.OrdersLineReadEvidence, lineOf("company_a", "uni_a")), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, cma, authz.OrdersLineReadEvidence, lineOf("company_a", "club_a")), authz.ErrMFARequired, false)
	cla := persona(t, "club_admin")
	cla.MFAVerified = false
	wantErr(t, authz.Can(ctx, cla, authz.OrdersLineReadEvidence, lineOf("company_a", "club_a")), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "company_owner_a"), authz.OrdersLineReadEvidence, lineOf("company_a", "club_a")), nil, false)
	wantErr(t, authz.Can(ctx, cs, authz.OrdersLineListSales, orgOf("uni_a", "university")), nil, false)

	// A seller of one line of a two-seller order sees the order (its own lines: the service).
	wantErr(t, authz.Can(ctx, persona(t, "other_club_admin"), authz.OrdersOrderRead, orderOf("company_a", "club_a", "club_b")), nil, false)
	// A malformed seller list hides the order from sellers.
	bad := orderOf("company_a", "club_a")
	bad.Attrs[authz.AttrOrderSellerOrgIDs] = "not-a-uuid"
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.OrdersOrderRead, bad), authz.ErrNotFound, false)

	// The system actions of the jobs (03 §3.7) and only them.
	for _, act := range []authz.Action{authz.OrdersOrderExpire, authz.OrdersLineAutoFulfill} {
		wantErr(t, authz.Can(ctx, authz.SystemActor("orders.job"), act, authz.Resource{Type: authz.TypeOrder}), nil, false)
		wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), act, authz.Resource{Type: authz.TypeOrder}), authz.ErrForbidden, false)
	}
	wantErr(t, authz.Can(ctx, authz.SystemActor("orders.job"), authz.OrdersOrderCancel, order0()), authz.ErrForbidden, false)

	// Missing Attrs keys deny (03 §3.5, 06 H-27).
	noSellers := orderOf("company_a", "club_a")
	delete(noSellers.Attrs, authz.AttrOrderSellerOrgIDs)
	wantErr(t, authz.Can(ctx, persona(t, "company_admin"), authz.OrdersOrderRead, noSellers), authz.ErrForbidden, true)
}

func order0() authz.Resource { return orderOf("company_a", "club_a") }
