package authz_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// The resources the notify service builds (03 §3.5; 06 H-27).
func ownedBy(typ, owner string) authz.Resource {
	id := authztest.ID(owner)
	return authz.Resource{Type: typ, OwnerUserID: &id}
}

func deliveryLog(subject, reason string) authz.Resource {
	id := authztest.ID(subject)
	return authz.Resource{Type: authz.TypeNotificationDelivery, ID: id, OwnerUserID: &id,
		Attrs: map[string]string{authz.AttrAccessReason: reason}}
}

func suppressionList(reason string) authz.Resource {
	return authz.Resource{Type: authz.TypeEmailSuppression, Attrs: map[string]string{authz.AttrAccessReason: reason}}
}

// TestNotifyPolicy_Matrix pins 03 §5.19 for the 18 personas (the HTTP
// matrix, testdata/authz/notify.yaml, checks the same through the API).
func TestNotifyPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden := authz.ErrForbidden
	const reason = "Ticket 4711: user misses e-mails"
	for _, tc := range []struct {
		name    string
		act     authz.Action
		res     func(p authztest.Persona) authz.Resource
		want    map[string]error
		missing error
	}{
		{"in_app.list_own", authz.NotifyInAppListOwn, func(p authztest.Persona) authz.Resource { return ownedBy(authz.TypeNotification, p.Name) },
			map[string]error{"anonymous": forbidden}, nil},
		{"in_app.mark_read", authz.NotifyInAppMarkRead, func(p authztest.Persona) authz.Resource { return ownedBy(authz.TypeNotification, p.Name) },
			map[string]error{"anonymous": forbidden}, nil},
		{"preference.read_own", authz.NotifyPreferenceReadOwn, func(p authztest.Persona) authz.Resource { return ownedBy(authz.TypeNotificationPref, p.Name) },
			map[string]error{"anonymous": forbidden}, nil},
		{"preference.update_own", authz.NotifyPreferenceUpdateOwn, func(p authztest.Persona) authz.Resource { return ownedBy(authz.TypeNotificationPref, p.Name) },
			map[string]error{"anonymous": forbidden}, nil},
		{"push_token.manage", authz.NotifyPushTokenManage, func(p authztest.Persona) authz.Resource { return ownedBy(authz.TypePushToken, p.Name) },
			map[string]error{"anonymous": forbidden}, nil},
		{"suppression.manage no reason", authz.NotifySuppressionManage, func(authztest.Persona) authz.Resource { return suppressionList("") },
			map[string]error{"anonymous": forbidden, "support": authz.ErrBreakGlassReason, "platform_admin": authz.ErrBreakGlassReason}, forbidden},
		{"suppression.manage with reason", authz.NotifySuppressionManage, func(authztest.Persona) authz.Resource { return suppressionList(reason) },
			map[string]error{"anonymous": forbidden, "support": nil, "platform_admin": nil}, forbidden},
		{"delivery.list no reason", authz.NotifyDeliveryList, func(authztest.Persona) authz.Resource { return deliveryLog("student", "") },
			map[string]error{"anonymous": forbidden, "support": authz.ErrBreakGlassReason, "platform_admin": authz.ErrBreakGlassReason}, forbidden},
		{"delivery.list with reason", authz.NotifyDeliveryList, func(authztest.Persona) authz.Resource { return deliveryLog("student", reason) },
			map[string]error{"anonymous": forbidden, "support": nil, "platform_admin": nil}, forbidden},
		{"provider_webhook.receive", authz.NotifyProviderWebhookReceive, func(authztest.Persona) authz.Resource {
			return authz.Resource{Type: authz.TypeEmailWebhook}
		}, nil, nil},
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

func TestNotifyPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// O: another user's inbox, preferences and tokens are invisible, staff included.
	for _, name := range []string{"student_minor", "support", "platform_admin", "dpo"} {
		for _, c := range []struct {
			act authz.Action
			typ string
		}{
			{authz.NotifyInAppListOwn, authz.TypeNotification}, {authz.NotifyInAppMarkRead, authz.TypeNotification},
			{authz.NotifyPreferenceUpdateOwn, authz.TypeNotificationPref}, {authz.NotifyPushTokenManage, authz.TypePushToken},
		} {
			wantErr(t, authz.Can(ctx, persona(t, name), c.act, ownedBy(c.typ, "student")), authz.ErrNotFound, false)
		}
	}
	// The wrong resource type is hidden.
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.NotifyInAppListOwn, ownedBy(authz.TypePushToken, "student")), authz.ErrNotFound, false)
	// B needs MFA (k-mfa) and a reason of 10–500 characters.
	sup := persona(t, "support")
	sup.MFAVerified = false
	wantErr(t, authz.Can(ctx, sup, authz.NotifyDeliveryList, deliveryLog("student", "Ticket 4711: misses e-mails")), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, persona(t, "support"), authz.NotifyDeliveryList, deliveryLog("student", "short")), authz.ErrBreakGlassReason, false)
	// A delivery log without a subject is nobody's (programmer error → hidden).
	wantErr(t, authz.Can(ctx, persona(t, "support"), authz.NotifyDeliveryList,
		authz.Resource{Type: authz.TypeNotificationDelivery, Attrs: map[string]string{authz.AttrAccessReason: "Ticket 4711: misses e-mails"}}),
		authz.ErrNotFound, false)
	// The system actor may only send (03 §3.7).
	wantErr(t, authz.Can(ctx, authz.SystemActor("notify.send_audit_alerts"), authz.NotifyMessageSend, authz.Resource{}), nil, false)
	wantErr(t, authz.Can(ctx, authz.SystemActor("notify.send_audit_alerts"), authz.NotifyDeliveryList, deliveryLog("student", "x")), authz.ErrForbidden, false)
}
