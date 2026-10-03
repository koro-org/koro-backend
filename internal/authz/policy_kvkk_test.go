package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// dsrOf is the resource the kvkk service builds for a request (03 §3.5;
// 06 H-27): owner = the data subject; type, status, first_approver always.
func dsrOf(owner, typ, status, first string, extra ...string) authz.Resource {
	id := authztest.ID(owner)
	attrs := map[string]string{"type": typ, authz.AttrStatus: status, authz.AttrFirstApprover: first}
	for i := 0; i+1 < len(extra); i += 2 {
		attrs[extra[i]] = extra[i+1]
	}
	return authz.Resource{Type: authz.TypeDSR, ID: uuid.NewV7(), OwnerUserID: &id, Attrs: attrs}
}

// dsrSurface is the admin pre-check resource (approve carries first_approver "").
var dsrSurface = authz.Resource{Type: authz.TypeAdminSurface, Attrs: map[string]string{authz.AttrFirstApprover: ""}}

// TestKvkkPolicy_Matrix pins 03 §5.22 (data subject requests) for the 18
// personas; testdata/authz/kvkk.yaml checks the same through the API.
func TestKvkkPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	dpoOnly := map[string]error{"anonymous": forbidden, "dpo": nil}
	dpoAndAdmin := map[string]error{"anonymous": forbidden, "dpo": nil, "platform_admin": nil}
	// On a request row (not the admin pre-check, which answers 403 first)
	// platform_admin does not see what is not awaiting the second approval.
	dpoRow := map[string]error{"anonymous": forbidden, "dpo": nil, "platform_admin": notFound}
	own := func(p authztest.Persona) authz.Resource { return dsrOf(p.Name, "access", "received", "") }
	for _, tc := range []struct {
		name    string
		act     authz.Action
		res     func(p authztest.Persona) authz.Resource
		want    map[string]error
		missing error
	}{
		{"create own", authz.KvkkDsrCreate, own, map[string]error{"anonymous": forbidden}, nil},
		{"list_own own", authz.KvkkDsrListOwn, own, map[string]error{"anonymous": forbidden}, nil},
		{"read_own own", authz.KvkkDsrReadOwn, own, map[string]error{"anonymous": forbidden}, nil},
		{"cancel_own own", authz.KvkkDsrCancelOwn, own, map[string]error{"anonymous": forbidden}, nil},
		{"export_download own", authz.KvkkDsrExportDownload, own, map[string]error{"anonymous": forbidden}, nil},
		{"export_download student's (admin surface)", authz.KvkkDsrExportDownload,
			func(authztest.Persona) authz.Resource { return dsrOf("student", "export", "completed", "") },
			map[string]error{"anonymous": forbidden, "student": nil, "dpo": nil, "platform_admin": notFound}, forbidden},
		{"list", authz.KvkkDsrList, func(authztest.Persona) authz.Resource { return dsrSurface }, dpoAndAdmin, forbidden},
		{"read received", authz.KvkkDsrRead,
			func(authztest.Persona) authz.Resource { return dsrOf("student", "access", "received", "") },
			map[string]error{"anonymous": forbidden, "dpo": nil, "platform_admin": notFound}, forbidden},
		{"read awaiting", authz.KvkkDsrRead,
			func(authztest.Persona) authz.Resource {
				return dsrOf("student", "profiling_access", "awaiting_second_approval", authztest.ID("dpo_2").String())
			}, dpoAndAdmin, forbidden},
		{"create_admin", authz.KvkkDsrCreateAdmin, func(authztest.Persona) authz.Resource { return dsrSurface }, dpoOnly, forbidden},
		{"update", authz.KvkkDsrUpdate,
			func(authztest.Persona) authz.Resource { return dsrOf("student", "access", "received", "") }, dpoRow, forbidden},
		{"complete", authz.KvkkDsrComplete,
			func(authztest.Persona) authz.Resource { return dsrOf("student", "access", "in_progress", "") }, dpoRow, forbidden},
		{"reject", authz.KvkkDsrReject,
			func(authztest.Persona) authz.Resource { return dsrOf("student", "access", "received", "") }, dpoRow, forbidden},
		{"approve first", authz.KvkkDsrApprove,
			func(authztest.Persona) authz.Resource {
				return dsrOf("student", "profiling_access", "received", "", "stage", "first")
			},
			map[string]error{"anonymous": forbidden, "dpo": nil, "platform_admin": notFound}, forbidden},
		{"approve second", authz.KvkkDsrApprove,
			func(authztest.Persona) authz.Resource {
				return dsrOf("student", "profiling_access", "awaiting_second_approval", authztest.ID("dpo_2").String(), "stage", "second")
			}, dpoAndAdmin, forbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = tc.missing
				}
				t.Run(p.Name, func(t *testing.T) {
					wantErr(t, authz.Can(ctx, p.Actor, tc.act, tc.res(p)), want, false)
				})
			}
		})
	}
}

// TestApproveProfilingAccess_SameDPOTwice_FourEyesRejected (03 §6.1): the
// first approver can never give the second approval, dpo or platform_admin.
func TestApproveProfilingAccess_SameDPOTwice_FourEyesRejected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, name := range []string{"dpo", "platform_admin"} {
		a := persona(t, name)
		second := dsrOf("student", "profiling_access", "awaiting_second_approval", a.UserID.String(), "stage", "second")
		wantErr(t, authz.Can(ctx, a, authz.KvkkDsrApprove, second), authz.ErrFourEyes, false)
	}
	// Another DPO may.
	second := dsrOf("student", "profiling_access", "awaiting_second_approval", authztest.ID("dpo").String(), "stage", "second")
	dpo2, _ := authztest.Get("dpo_2")
	wantErr(t, authz.Can(ctx, dpo2.Actor, authz.KvkkDsrApprove, second), nil, false)
}

func TestKvkkPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// O: another user's request is invisible on /me, staff included.
	for _, name := range []string{"student_b", "dpo", "platform_admin"} {
		for _, act := range []authz.Action{authz.KvkkDsrReadOwn, authz.KvkkDsrCancelOwn, authz.KvkkDsrCreate} {
			wantErr(t, authz.Can(ctx, persona(t, name), act, dsrOf("student", "access", "received", "")), authz.ErrNotFound, false)
		}
	}
	// A request without its mandatory Attrs is a programmer error.
	id := authztest.ID("student")
	bare := authz.Resource{Type: authz.TypeDSR, OwnerUserID: &id, Attrs: map[string]string{"type": "access"}}
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.KvkkDsrReadOwn, bare), authz.ErrForbidden, true)
	// dpo without an MFA-verified session.
	dpo := persona(t, "dpo")
	dpo.MFAVerified = false
	wantErr(t, authz.Can(ctx, dpo, authz.KvkkDsrList, dsrSurface), authz.ErrMFARequired, false)
	// Retention, breach and subprocessor actions stay denied until BE-049.
	for _, act := range []authz.Action{authz.KvkkRetentionPolicyList, authz.KvkkBreachManage, authz.KvkkSubprocessorList} {
		wantErr(t, authz.Can(ctx, persona(t, "dpo"), act, adminSurface), authz.ErrNotFound, false)
	}
}
