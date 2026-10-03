// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the audit module (03 §5.21), in table order.
const (
	AuditEventListOwn Action = "audit.event.list_own"
	AuditEventListOrg Action = "audit.event.list_org"
	AuditEventList    Action = "audit.event.list"
	AuditChainVerify  Action = "audit.chain.verify"
)

// System-only actions of the audit module (03 §3.7): only SystemActor may perform them.
const (
	AuditChainAppend Action = "audit.chain.append"
)

// auditActions lists the HTTP actions of audit in 03 §5.21 order.
var auditActions = []Action{
	AuditEventListOwn,
	AuditEventListOrg,
	AuditEventList,
	AuditChainVerify,
}
