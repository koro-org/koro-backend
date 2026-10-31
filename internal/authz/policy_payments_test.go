package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// paymentsAccount is a collection account created by creator.
func paymentsAccount(creator uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeCollectionAccount, ID: authztest.ID("coll"), Attrs: map[string]string{
		authz.AttrStatus: "pending_approval", authz.AttrCreatedBy: creator.String(),
	}}
}

// paymentsTransfer is an incoming transfer whose return was requested by
// requester (empty: the first step or the receipt).
func paymentsTransfer(requester string) authz.Resource {
	return authz.Resource{Type: authz.TypeIncomingTransfer, ID: authztest.ID("tr"), Attrs: map[string]string{
		authz.AttrStatus: "return_requested", authz.AttrRequestedBy: requester,
	}}
}

// paymentsRefund is a refund requested by requester.
func paymentsRefund(requester uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeRefund, ID: authztest.ID("refund"), Attrs: map[string]string{
		authz.AttrStatus: "requested", authz.AttrRequestedBy: requester.String(),
	}}
}

// TestPaymentsPolicy_Matrix pins 03 §5.16 (BE-041's and BE-043's parts) for the 18
// personas: the admin surface (403 before reading), the second eye on a
// collection account created by finance_admin_2 and the return steps (the
// HTTP matrix, testdata/authz/payments.yaml, checks the same through the API).
func TestPaymentsPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	staff := withAnon(map[string]error{"platform_admin": nil, "finance_admin": nil})
	finOnly := withAnon(map[string]error{"finance_admin": nil})
	cases := []struct {
		act  authz.Action
		r    authz.Resource
		want map[string]error
	}{
		{authz.PaymentsCollectionAccountList, adminSurface, staff},
		{authz.PaymentsCollectionAccountCreate, adminSurface, finOnly},
		{authz.PaymentsCollectionAccountApprove, paymentsAccount(authztest.ID("finance_admin_2")), staff},
		{authz.PaymentsCollectionAccountRetire, adminSurface, staff},
		{authz.PaymentsTransferList, adminSurface, staff},
		{authz.PaymentsTransferRead, adminSurface, staff},
		{authz.PaymentsTransferCreate, adminSurface, finOnly},
		{authz.PaymentsTransferMatch, adminSurface, finOnly},
		{authz.PaymentsTransferIgnore, adminSurface, finOnly},
		{authz.PaymentsTransferMarkReturned, paymentsTransfer(""), finOnly},
		{authz.PaymentsTransferMarkReturned, paymentsTransfer(authztest.ID("finance_admin_2").String()), finOnly},
		{authz.PaymentsPaymentList, adminSurface, staff},
		{authz.PaymentsPaymentRead, adminSurface, staff},
		{authz.PaymentsRefundList, adminSurface, staff},
		{authz.PaymentsRefundCreate, adminSurface, staff},
		{authz.PaymentsRefundApprove, paymentsRefund(authztest.ID("finance_admin_2")), finOnly},
		{authz.PaymentsRefundReject, adminSurface, finOnly},
		{authz.PaymentsRefundComplete, adminSurface, finOnly},
		{authz.PaymentsIbanReveal, adminSurface, staff}, // BE-129
	}
	for _, c := range cases {
		for _, p := range authztest.Personas() {
			w, ok := c.want[p.Name]
			if !ok {
				w = authz.ErrForbidden // admin surface: 403, never 404
			}
			err := authz.Can(ctx, p.Actor, c.act, c.r)
			if (err == nil) != (w == nil) {
				t.Errorf("%s / %s: Can() = %v, want %v", c.act, p.Name, err, w)
				continue
			}
			wantErr(t, err, w, false)
		}
	}
}

func TestPaymentsPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fin, pad := persona(t, "finance_admin"), persona(t, "platform_admin")

	// Four eyes (03 §6.1): the creator of an account never decides it.
	wantErr(t, authz.Can(ctx, fin, authz.PaymentsCollectionAccountApprove, paymentsAccount(fin.UserID)), authz.ErrFourEyes, false)
	wantErr(t, authz.Can(ctx, pad, authz.PaymentsCollectionAccountApprove, paymentsAccount(pad.UserID)), authz.ErrFourEyes, false)
	// The second eye of a return is never its requester (06 H-18).
	wantErr(t, authz.Can(ctx, fin, authz.PaymentsTransferMarkReturned, paymentsTransfer(fin.UserID.String())), authz.ErrFourEyes, false)
	// A service that forgets the four-eyes keys is refused (fail closed).
	wantErr(t, authz.Can(ctx, fin, authz.PaymentsTransferMarkReturned, authz.Resource{Type: authz.TypeIncomingTransfer}),
		authz.ErrForbidden, true)
	wantErr(t, authz.Can(ctx, pad, authz.PaymentsCollectionAccountApprove, adminSurface), authz.ErrForbidden, true)

	// k-mfa: platform_admin and finance_admin need an MFA session.
	for _, a := range []authz.Actor{fin, pad} {
		a.MFAVerified = false
		wantErr(t, authz.Can(ctx, a, authz.PaymentsTransferList, adminSurface), authz.ErrMFARequired, false)
	}

	// Actions bound to the other resource type are not visible there.
	wantErr(t, authz.Can(ctx, fin, authz.PaymentsTransferMatch, paymentsTransfer("")), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, fin, authz.PaymentsTransferList, paymentsAccount(pad.UserID)), authz.ErrNotFound, false)

	// Four eyes on a refund (03 §6.1 "İade"): its requester never approves
	// it — a platform admin's request included (pad may request, only fin
	// approves); a refund without requested_by is refused (fail closed); the
	// approval is never on the admin surface.
	wantErr(t, authz.Can(ctx, fin, authz.PaymentsRefundApprove, paymentsRefund(fin.UserID)), authz.ErrFourEyes, false)
	if err := authz.Can(ctx, fin, authz.PaymentsRefundApprove, paymentsRefund(pad.UserID)); err != nil {
		t.Errorf("finance_admin approving a platform admin's refund: %v", err)
	}
	wantErr(t, authz.Can(ctx, pad, authz.PaymentsRefundApprove, paymentsRefund(fin.UserID)), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, fin, authz.PaymentsRefundApprove, authz.Resource{Type: authz.TypeRefund,
		Attrs: map[string]string{authz.AttrStatus: "requested", authz.AttrRequestedBy: ""}}), authz.ErrForbidden, true)
	wantErr(t, authz.Can(ctx, fin, authz.PaymentsRefundApprove, adminSurface), authz.ErrForbidden, true)
	// The other refund actions are not on the refund resource.
	wantErr(t, authz.Can(ctx, fin, authz.PaymentsRefundComplete, paymentsRefund(pad.UserID)), authz.ErrNotFound, false)
	// No system actor moves a refund (03 §3.7).
	wantErr(t, authz.Can(ctx, authz.SystemActor("payments.unmatched_digest"), authz.PaymentsRefundComplete, adminSurface),
		authz.ErrForbidden, false)

	// No system actor captures a payment (03 §3.7).
	wantErr(t, authz.Can(ctx, authz.SystemActor("payments.unmatched_digest"), authz.PaymentsTransferMatch, adminSurface),
		authz.ErrForbidden, false)
}

// refundRequestOf is the Resource the payments service builds of a refund
// request (BE-132): OrgID = the buyer; Attrs status, the line's seller and
// the requester.
func refundRequestOf(buyer, seller string, requester uuid.UUID) authz.Resource {
	id := authztest.ID(buyer)
	return authz.Resource{Type: authz.TypeRefundRequest, ID: uuid.NewV7(), OrgID: &id, Attrs: map[string]string{
		authz.AttrStatus: "open", authz.AttrOrderLineSellerOrgID: authztest.ID(seller).String(),
		authz.AttrRequestedBy: requester.String(),
	}}
}

// TestPaymentsPolicy_RefundRequests pins BE-132's cells for the 18
// personas: the buyer's org surface (cma asks; cmm and cma list), the
// seller's (its club admin, university rep and career staff list, as
// orders.line.list_sales), one request (the buyer's members, the seller's
// staff, sup, pad, fin read; the buyer's admin withdraws; the evidence is
// the parties' only: the buyer's admin, the seller's markers, pad, fin) and
// the decisions (admin surface pad, fin; on the request never its
// requester).
func TestPaymentsPolicy_RefundRequests(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
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
	both := append(append([]string{}, companyA...), "club_member", "club_admin")
	req := refundRequestOf("company_a", "club_a", authztest.ID("company_admin"))
	cells := []struct {
		name string
		act  authz.Action
		r    authz.Resource
		want map[string]error
	}{
		{"create", authz.PaymentsRefundRequestCreate, orgOf("company_a", "company"), surface(companyA, map[string]error{"company_admin": nil})},
		{"create for a club (no company)", authz.PaymentsRefundRequestCreate, orgOf("club_a", "club"),
			surface([]string{"club_member", "club_admin"}, nil)},
		{"create for another company (IDOR)", authz.PaymentsRefundRequestCreate, orgOf("company_b", "company"),
			surface([]string{"other_company_member"}, map[string]error{"other_company_member": nil})},
		{"list the buyer's", authz.PaymentsRefundRequestList, orgOf("company_a", "company"),
			surface(companyA, map[string]error{"company_member": nil, "company_admin": nil})},
		{"list a club's sales", authz.PaymentsRefundRequestListSales, orgOf("club_a", "club"),
			surface([]string{"club_member", "club_admin"}, map[string]error{"club_admin": nil, "platform_admin": nil, "finance_admin": nil})},
		{"list a university's sales", authz.PaymentsRefundRequestListSales, orgOf("uni_a", "university"),
			surface([]string{"university_rep"}, map[string]error{"university_rep": nil, "platform_admin": nil, "finance_admin": nil})},
		{"read", authz.PaymentsRefundRequestRead, req, surface(both, map[string]error{"company_member": nil, "company_admin": nil,
			"club_admin": nil, "support": nil, "platform_admin": nil, "finance_admin": nil})},
		{"read another company's (IDOR)", authz.PaymentsRefundRequestRead, refundRequestOf("company_b", "club_b", authztest.ID("other_company_member")),
			surface([]string{"other_company_member", "other_club_admin"}, map[string]error{"other_company_member": nil,
				"other_club_admin": nil, "support": nil, "platform_admin": nil, "finance_admin": nil})},
		{"withdraw", authz.PaymentsRefundRequestWithdraw, req, surface(both, map[string]error{"company_admin": nil})},
		{"replace the evidence (create on the request, BE-154b)", authz.PaymentsRefundRequestCreate, req,
			surface(both, map[string]error{"company_admin": nil})},
		{"read the evidence", authz.PaymentsRefundRequestReadEvidence, req, surface(both, map[string]error{"company_admin": nil,
			"club_admin": nil, "platform_admin": nil, "finance_admin": nil})},
		{"read a university's evidence (k-cs)", authz.PaymentsRefundRequestReadEvidence, refundRequestOf("company_a", "uni_a", authztest.ID("company_admin")),
			surface(append([]string{"university_rep"}, companyA...), map[string]error{"company_admin": nil, "university_rep": nil,
				"platform_admin": nil, "finance_admin": nil})},
	}
	for _, act := range []authz.Action{authz.PaymentsRefundRequestReview, authz.PaymentsRefundRequestAccept, authz.PaymentsRefundRequestReject} {
		m := map[string]error{}
		for _, p := range authztest.Personas() {
			m[p.Name] = forbidden // the admin surface's visibility: 403, never 404
		}
		m["platform_admin"], m["finance_admin"] = nil, nil
		cells = append(cells, struct {
			name string
			act  authz.Action
			r    authz.Resource
			want map[string]error
		}{"decide " + string(act), act, req, m})
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
	// The admin surface: 403 before reading for everyone without the role.
	for _, act := range []authz.Action{authz.PaymentsRefundRequestListAdmin, authz.PaymentsRefundRequestReview,
		authz.PaymentsRefundRequestAccept, authz.PaymentsRefundRequestReject} {
		for _, p := range authztest.Personas() {
			var want error = forbidden
			if p.Name == "platform_admin" || p.Name == "finance_admin" {
				want = nil
			}
			wantErr(t, authz.Can(ctx, p.Actor, act, adminSurface), want, false)
		}
	}
	// Four eyes: a staff member never decides its own request; a request
	// without requested_by is refused (fail closed).
	fin := persona(t, "finance_admin")
	for _, act := range []authz.Action{authz.PaymentsRefundRequestReview, authz.PaymentsRefundRequestAccept, authz.PaymentsRefundRequestReject} {
		wantErr(t, authz.Can(ctx, fin, act, refundRequestOf("company_a", "club_a", fin.UserID)), authz.ErrFourEyes, false)
	}
	bare := refundRequestOf("company_a", "club_a", fin.UserID)
	delete(bare.Attrs, authz.AttrRequestedBy)
	wantErr(t, authz.Can(ctx, fin, authz.PaymentsRefundRequestAccept, bare), authz.ErrForbidden, true)
	// The company owner asks like the admin; career staff reads the
	// university's requests but never its evidence (k-cs).
	owner := persona(t, "company_owner_a")
	if err := authz.Can(ctx, owner, authz.PaymentsRefundRequestCreate, orgOf("company_a", "company")); err != nil {
		t.Errorf("company owner creating: %v", err)
	}
	uni := authztest.ID("uni_a")
	career := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), MFAVerified: true,
		Memberships: []authz.Membership{{OrgID: uni, OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	uniReq := refundRequestOf("company_a", "uni_a", authztest.ID("company_admin"))
	if err := authz.Can(ctx, career, authz.PaymentsRefundRequestRead, uniReq); err != nil {
		t.Errorf("career staff reading: %v", err)
	}
	wantErr(t, authz.Can(ctx, career, authz.PaymentsRefundRequestReadEvidence, uniReq), authz.ErrForbidden, false)
}
