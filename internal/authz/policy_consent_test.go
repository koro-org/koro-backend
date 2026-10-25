package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// The resources the consent service builds (03 §3.5; 06 H-27).
func noticeVersion(status string) authz.Resource {
	return authz.Resource{Type: authz.TypeNoticeVersion, ID: uuid.NewV7(), Attrs: map[string]string{authz.AttrStatus: status}}
}

func consentRecord(owner string, reason string) authz.Resource {
	id := authztest.ID(owner)
	return authz.Resource{Type: authz.TypeConsentRecord, OwnerUserID: &id, Attrs: map[string]string{authz.AttrAccessReason: reason}}
}

// TestConsentPolicy_Matrix pins 03 §5.5 for the 18 personas (the HTTP
// matrix, testdata/authz/consent.yaml, checks the same through the API).
func TestConsentPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden := authz.ErrForbidden
	dpoOnly := map[string]error{"anonymous": forbidden, "dpo": nil}
	for _, tc := range []struct {
		name    string
		act     authz.Action
		res     func(p authztest.Persona) authz.Resource
		want    map[string]error
		missing error // personas not in want
	}{
		{"notice.read published", authz.ConsentNoticeRead, func(authztest.Persona) authz.Resource { return noticeVersion("published") }, nil, nil},
		{"notice.read retired", authz.ConsentNoticeRead, func(authztest.Persona) authz.Resource { return noticeVersion("retired") }, nil, nil},
		{"notice.read draft", authz.ConsentNoticeRead, func(authztest.Persona) authz.Resource { return noticeVersion("draft") },
			map[string]error{"dpo": nil}, authz.ErrNotFound},
		{"record.create own", authz.ConsentRecordCreate, func(p authztest.Persona) authz.Resource { return consentRecord(p.Name, "") },
			map[string]error{"anonymous": forbidden}, nil},
		{"record.list_own own", authz.ConsentRecordListOwn, func(p authztest.Persona) authz.Resource { return consentRecord(p.Name, "") },
			map[string]error{"anonymous": forbidden}, nil},
		{"record.list_admin no reason", authz.ConsentRecordListAdmin, func(authztest.Persona) authz.Resource { return consentRecord("student", "") },
			map[string]error{"anonymous": forbidden, "dpo": nil, "support": authz.ErrBreakGlassReason}, forbidden},
		{"record.list_admin with reason", authz.ConsentRecordListAdmin,
			func(authztest.Persona) authz.Resource {
				return consentRecord("student", "Ticket 4711: unsubscribe fails")
			},
			map[string]error{"anonymous": forbidden, "dpo": nil, "support": nil}, forbidden},
		{"notice.manage", authz.ConsentNoticeManage, func(authztest.Persona) authz.Resource {
			return authz.Resource{Type: authz.TypeNoticeVersion}
		}, dpoOnly, forbidden},
		{"purpose.manage", authz.ConsentPurposeManage, func(authztest.Persona) authz.Resource { return adminSurface }, dpoOnly, forbidden},
		// BE-110: the integrator's webhook is public; its identity is the
		// URL's secret, checked by the İYS adapter.
		{"iys_webhook.receive", authz.ConsentIysWebhookReceive, func(authztest.Persona) authz.Resource {
			return authz.Resource{Type: authz.TypeIYSWebhook}
		}, nil, nil},
		{"iys_webhook.receive other type", authz.ConsentIysWebhookReceive, func(authztest.Persona) authz.Resource {
			return authz.Resource{Type: authz.TypeEmailWebhook}
		}, nil, authz.ErrNotFound},
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

func TestConsentPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// O: another user's ledger is invisible, staff included.
	for _, name := range []string{"student_minor", "dpo", "support", "platform_admin"} {
		wantErr(t, authz.Can(ctx, persona(t, name), authz.ConsentRecordCreate, consentRecord("student", "")), authz.ErrNotFound, false)
		wantErr(t, authz.Can(ctx, persona(t, name), authz.ConsentRecordListOwn, consentRecord("student", "")), authz.ErrNotFound, false)
	}
	// A consent_record without an owner is nobody's (programmer error → hidden).
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.ConsentRecordListOwn, authz.Resource{Type: authz.TypeConsentRecord}), authz.ErrNotFound, false)

	// A notice without its status Attrs is hidden (safe default), even from dpo.
	bare := authz.Resource{Type: authz.TypeNoticeVersion, ID: uuid.NewV7()}
	wantErr(t, authz.Can(ctx, persona(t, "dpo"), authz.ConsentNoticeRead, bare), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, authz.Anonymous(), authz.ConsentNoticeRead, noticeVersion("published")), nil, false)

	// k-mfa: dpo needs an MFA session to manage, and to see drafts.
	dpo := persona(t, "dpo")
	dpo.MFAVerified = false
	wantErr(t, authz.Can(ctx, dpo, authz.ConsentNoticeManage, authz.Resource{Type: authz.TypeNoticeVersion}), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, dpo, authz.ConsentPurposeManage, adminSurface), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, dpo, authz.ConsentRecordListAdmin, consentRecord("student", "")), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, dpo, authz.ConsentNoticeRead, noticeVersion("draft")), authz.ErrNotFound, false)

	// B: support's reason is 10–500 characters; an MFA-less support session is refused first.
	sup := persona(t, "support")
	wantErr(t, authz.Can(ctx, sup, authz.ConsentRecordListAdmin, consentRecord("student", "too short")), authz.ErrBreakGlassReason, false)
	sup.MFAVerified = false
	wantErr(t, authz.Can(ctx, sup, authz.ConsentRecordListAdmin, consentRecord("student", "Ticket 4711: unsubscribe fails")), authz.ErrMFARequired, false)

	// The actions are bound to their resource types.
	wantErr(t, authz.Can(ctx, persona(t, "dpo"), authz.ConsentPurposeManage, noticeVersion("draft")), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "dpo"), authz.ConsentNoticeManage, adminSurface), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "dpo"), authz.ConsentRecordListAdmin, authz.Resource{Type: authz.TypeConsentRecord}), authz.ErrNotFound, false)

	// No consent action is a system action (03 §3.7).
	wantErr(t, authz.Can(ctx, authz.SystemActor("consent.iys_reconcile"), authz.ConsentRecordCreate, consentRecord("student", "")), authz.ErrForbidden, false)
}
