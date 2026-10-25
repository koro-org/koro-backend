// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the consent module (03 §5.5), in table order.
const (
	ConsentNoticeRead      Action = "consent.notice.read"
	ConsentRecordCreate    Action = "consent.record.create"
	ConsentRecordListOwn   Action = "consent.record.list_own"
	ConsentRecordListAdmin Action = "consent.record.list_admin"
	ConsentNoticeManage    Action = "consent.notice.manage"
	ConsentPurposeManage   Action = "consent.purpose.manage"
	// ConsentIysWebhookReceive is the webhook of an İYS integrator that
	// pushes İYS's answers and changes (BE-110: Netgsm; 03 §5.5 doc patch).
	ConsentIysWebhookReceive Action = "consent.iys_webhook.receive"
)

// consentActions lists the HTTP actions of consent in 03 §5.5 order.
var consentActions = []Action{
	ConsentNoticeRead,
	ConsentRecordCreate,
	ConsentRecordListOwn,
	ConsentRecordListAdmin,
	ConsentNoticeManage,
	ConsentPurposeManage,
	ConsentIysWebhookReceive,
}
