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
