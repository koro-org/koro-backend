// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// System-only actions of the platform webhook inbox (not one of the 22 modules) (03 §3.7): only SystemActor may perform them.
const (
	WebhookInboxProcess Action = "webhook.inbox.process"
)
