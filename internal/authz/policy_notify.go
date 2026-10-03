package authz

// Resource types of the notify module beyond 03 §3.5 (Policy.Types).
const (
	// TypeNotificationDelivery is the delivery log of one user
	// (adminListUserDeliveries): OwnerUserID = the user whose log is read,
	// Attrs access_reason (break-glass).
	TypeNotificationDelivery = "notification_delivery"
	// TypeEmailSuppression is the e-mail suppression list
	// (adminListEmailSuppressions, adminDeleteEmailSuppression): Attrs
	// access_reason (break-glass).
	TypeEmailSuppression = "email_suppression"
	// TypeEmailWebhook is a provider webhook delivery (receiveEmailWebhook).
	TypeEmailWebhook = "email_webhook"
)

// notifyPolicy is the policy of the notify module (03 §5.19). Only tickets of
// the notify module edit this file (BE-001 R5). Written by BE-015.
//
//	notify.in_app.list_own           O for every signed-in user (/me)
//	notify.in_app.mark_read          O
//	notify.preference.read_own       O
//	notify.preference.update_own     O (security cannot be switched off: service, 422)
//	notify.push_token.manage         O
//	notify.suppression.manage        support B, platform_admin B
//	notify.delivery.list             support B, platform_admin B
//	notify.provider_webhook.receive  P (identity = the provider signature, checked by the service)
//
// MFA follows from the role paths (support, platform_admin: k-mfa).
// notify.message.send is a system action (03 §3.7), granted by the core's
// allow-list only.
//
// Resources (03 §3.5; 06 H-27): the O actions pass notification,
// notification_pref or push_token with OwnerUserID = the owner (the actor
// on /me; another user's row is not found, 03 §7.2); the B actions pass
// notification_delivery (OwnerUserID = the user whose log is read) or
// email_suppression, both with Attrs access_reason. The /admin operations
// are the admin surface: visible to every user, 403 without the role
// (03 §7.2-3).
func notifyPolicy() Policy {
	return Policy{
		Types: map[string][]string{
			TypeNotificationDelivery: {AttrAccessReason},
			TypeEmailSuppression:     {AttrAccessReason},
			TypeEmailWebhook:         nil,
		},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case NotifyInAppListOwn, NotifyInAppMarkRead:
				return r.Type == TypeNotification && a.Owns(r)
			case NotifyPreferenceReadOwn, NotifyPreferenceUpdateOwn:
				return r.Type == TypeNotificationPref && a.Owns(r)
			case NotifyPushTokenManage:
				return r.Type == TypePushToken && a.Owns(r)
			case NotifySuppressionManage:
				return r.Type == TypeEmailSuppression && AdminSurfaceVisible(a)
			case NotifyDeliveryList:
				return r.Type == TypeNotificationDelivery && r.OwnerUserID != nil && AdminSurfaceVisible(a)
			case NotifyProviderWebhookReceive:
				return r.Type == TypeEmailWebhook
			}
			return false
		},
		Grants: func(_ Actor, act Action, _ Resource) []Grant {
			switch act {
			case NotifyInAppListOwn, NotifyInAppMarkRead, NotifyPreferenceReadOwn, NotifyPreferenceUpdateOwn,
				NotifyPushTokenManage:
				return []Grant{{Role: RoleSelf}}
			case NotifySuppressionManage, NotifyDeliveryList:
				return []Grant{{Role: RoleSupport, BreakGlass: true}, {Role: RolePlatformAdmin, BreakGlass: true}}
			case NotifyProviderWebhookReceive:
				return []Grant{{Role: RolePublic}}
			}
			return nil
		},
	}
}
