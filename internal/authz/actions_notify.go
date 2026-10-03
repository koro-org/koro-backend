// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the notify module (03 §5.19), in table order.
const (
	NotifyInAppListOwn           Action = "notify.in_app.list_own"
	NotifyInAppMarkRead          Action = "notify.in_app.mark_read"
	NotifyPreferenceReadOwn      Action = "notify.preference.read_own"
	NotifyPreferenceUpdateOwn    Action = "notify.preference.update_own"
	NotifyPushTokenManage        Action = "notify.push_token.manage"
	NotifySuppressionManage      Action = "notify.suppression.manage"
	NotifyDeliveryList           Action = "notify.delivery.list"
	NotifyProviderWebhookReceive Action = "notify.provider_webhook.receive"
)

// System-only actions of the notify module (03 §3.7): only SystemActor may perform them.
const (
	NotifyMessageSend Action = "notify.message.send"
)

// notifyActions lists the HTTP actions of notify in 03 §5.19 order.
var notifyActions = []Action{
	NotifyInAppListOwn,
	NotifyInAppMarkRead,
	NotifyPreferenceReadOwn,
	NotifyPreferenceUpdateOwn,
	NotifyPushTokenManage,
	NotifySuppressionManage,
	NotifyDeliveryList,
	NotifyProviderWebhookReceive,
}
