package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// invoicesOf is the org's invoices as a collection (listOrgInvoices: the
// org is both the issuer and the recipient side).
func invoicesOf(org string) authz.Resource {
	id := authztest.ID(org)
	return authz.Resource{Type: authz.TypeInvoice, OrgID: &id,
		Attrs: map[string]string{"recipient_org_id": id.String(), "issuing_mode": "", authz.AttrUploadedBy: ""}}
}

// invoiceOf is one invoice issued by issuer's org (nil for Koro) to
// recipient's org, of mode, uploaded by uploader ("" none).
func invoiceOf(issuer, recipient, mode, uploader string) authz.Resource {
	r := authz.Resource{Type: authz.TypeInvoice, ID: uuid.NewV7(), Attrs: map[string]string{
		"recipient_org_id": authztest.ID(recipient).String(), "issuing_mode": mode, authz.AttrUploadedBy: "",
	}}
	if issuer != "" {
		id := authztest.ID(issuer)
		r.OrgID = &id
	}
	if uploader != "" {
		r.Attrs[authz.AttrUploadedBy] = authztest.ID(uploader).String()
	}
	return r
}

// TestBillingPolicy_Matrix pins 03 §5.18 for the 18 personas (the HTTP
// matrix, testdata/authz/billing.yaml, checks the same through the API).
func TestBillingPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	for _, c := range []struct {
		act  authz.Action
		want map[string]error // not listed: 403 (admin surface, never 404)
	}{
		{authz.BillingInvoiceListAdmin, withAnon(map[string]error{"platform_admin": nil, "finance_admin": nil})},
		{authz.BillingIntegrationManage, withAnon(map[string]error{"platform_admin": nil, "finance_admin": nil})},
		{authz.BillingInvoiceCancel, withAnon(map[string]error{"finance_admin": nil})},
	} {
		for _, p := range authztest.Personas() {
			w, ok := c.want[p.Name]
			if !ok {
				w = forbidden
			}
			wantErr(t, authz.Can(ctx, p.Actor, c.act, adminSurface), w, false)
		}
	}

	staff := map[string]error{
		"anonymous": forbidden, "moderator": forbidden, "support": forbidden, "dpo": forbidden, "platform_admin": nil,
		"finance_admin": nil, "talent_auditor": forbidden,
	}
	with := func(m map[string]error) map[string]error {
		out := map[string]error{}
		for k, v := range staff {
			out[k] = v
		}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	service := invoiceOf("club_a", "company_a", "seller_uploaded", "")
	commission := invoiceOf("", "club_a", "manual_upload", "")
	for _, c := range []struct {
		name string
		act  authz.Action
		r    authz.Resource
		want map[string]error // not listed: 404
	}{
		{"list club_a", authz.BillingInvoiceList, invoicesOf("club_a"),
			with(map[string]error{"club_member": forbidden, "club_admin": nil})},
		{"list uni_a", authz.BillingInvoiceList, invoicesOf("uni_a"), with(map[string]error{"university_rep": nil})},
		{"list company_a", authz.BillingInvoiceList, invoicesOf("company_a"),
			with(map[string]error{"company_member": nil, "company_admin": nil})},
		{"read the payee's service invoice", authz.BillingInvoiceRead, service,
			with(map[string]error{"club_member": forbidden, "club_admin": nil, "company_member": nil, "company_admin": nil})},
		{"read Koro's commission invoice", authz.BillingInvoiceRead, commission,
			with(map[string]error{"club_member": forbidden, "club_admin": nil})},
		{"upload the payee's service invoice (k29)", authz.BillingInvoiceUpload, service,
			with(map[string]error{"club_member": forbidden, "club_admin": nil, "company_member": forbidden,
				"company_admin": forbidden, "platform_admin": forbidden})},
		{"upload Koro's commission invoice", authz.BillingInvoiceUpload, commission,
			with(map[string]error{"club_member": forbidden, "club_admin": forbidden, "platform_admin": forbidden})},
	} {
		for _, p := range authztest.Personas() {
			want, ok := c.want[p.Name]
			if !ok {
				want = notFound
			}
			err := authz.Can(ctx, p.Actor, c.act, c.r)
			if want == nil && err != nil || want != nil && err == nil {
				t.Errorf("%s %s: %v, want %v", c.name, p.Name, err, want)
				continue
			}
			if want != nil {
				wantErr(t, err, want, false)
			}
		}
	}
}

// TestBillingPolicy_VerifyFourEyes pins 03 §6.1 "Fatura doğrulama" (06
// H-19): only a finance admin verifies, never the uploader; uploaded_by is
// mandatory (06 H-27, fail closed).
func TestBillingPolicy_VerifyFourEyes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, tc := range []struct {
		who  string
		r    authz.Resource
		want error
	}{
		{"finance_admin", invoiceOf("", "club_a", "manual_upload", "finance_admin"), authz.ErrFourEyes},
		{"finance_admin", invoiceOf("", "club_a", "manual_upload", "finance_admin_2"), nil},
		{"finance_admin", invoiceOf("club_a", "company_a", "seller_uploaded", "club_admin"), nil},
		{"platform_admin", invoiceOf("club_a", "company_a", "seller_uploaded", "club_admin"), authz.ErrForbidden},
		{"club_admin", invoiceOf("club_a", "company_a", "seller_uploaded", "club_admin"), authz.ErrNotFound},
	} {
		wantErr(t, authz.Can(ctx, persona(t, tc.who), authz.BillingInvoiceVerify, tc.r), tc.want, false)
	}
	empty := invoiceOf("", "club_a", "manual_upload", "")
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.BillingInvoiceVerify, empty), authz.ErrForbidden, true)
	noKey := invoiceOf("", "club_a", "manual_upload", "finance_admin_2")
	delete(noKey.Attrs, authz.AttrUploadedBy)
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.BillingInvoiceVerify, noKey), authz.ErrForbidden, true)
}

func TestBillingPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// k-mfa: finance, the university rep and the company admin need MFA; the
	// club admin and the recruiter do not.
	fin := persona(t, "finance_admin")
	fin.MFAVerified = false
	wantErr(t, authz.Can(ctx, fin, authz.BillingInvoiceListAdmin, adminSurface), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, fin, authz.BillingIntegrationManage, adminSurface), authz.ErrMFARequired, false)
	cma := persona(t, "company_admin")
	cma.MFAVerified = false
	wantErr(t, authz.Can(ctx, cma, authz.BillingInvoiceList, invoicesOf("company_a")), authz.ErrMFARequired, false)
	cla := persona(t, "club_admin")
	cla.MFAVerified = false
	wantErr(t, authz.Can(ctx, cla, authz.BillingInvoiceUpload, invoiceOf("club_a", "company_a", "seller_uploaded", "")), nil, false)
	// k29: a club admin uploads only its own org's service invoice.
	wantErr(t, authz.Can(ctx, persona(t, "other_club_admin"), authz.BillingInvoiceUpload,
		invoiceOf("club_a", "company_a", "seller_uploaded", "")), authz.ErrNotFound, false)
	// The admin writes are bound to the admin surface; the plan is the system's only.
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.BillingInvoiceCancel, invoicesOf("club_a")), authz.ErrNotFound, false)
	sys := authz.SystemActor("billing.plan_on_payment")
	wantErr(t, authz.Can(ctx, sys, authz.BillingInvoicePlan, invoiceOf("", "club_a", "", "")), nil, false)
	wantErr(t, authz.Can(ctx, sys, authz.BillingInvoiceCancel, adminSurface), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.BillingInvoicePlan, adminSurface), authz.ErrForbidden, false)
}
