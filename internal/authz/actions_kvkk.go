// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the kvkk module (03 §5.22), in table order.
const (
	KvkkDsrCreate             Action = "kvkk.dsr.create"
	KvkkDsrListOwn            Action = "kvkk.dsr.list_own"
	KvkkDsrReadOwn            Action = "kvkk.dsr.read_own"
	KvkkDsrCancelOwn          Action = "kvkk.dsr.cancel_own"
	KvkkDsrExportDownload     Action = "kvkk.dsr.export_download"
	KvkkDsrList               Action = "kvkk.dsr.list"
	KvkkDsrRead               Action = "kvkk.dsr.read"
	KvkkDsrCreateAdmin        Action = "kvkk.dsr.create_admin"
	KvkkDsrUpdate             Action = "kvkk.dsr.update"
	KvkkDsrApprove            Action = "kvkk.dsr.approve"
	KvkkDsrComplete           Action = "kvkk.dsr.complete"
	KvkkDsrReject             Action = "kvkk.dsr.reject"
	KvkkRetentionPolicyList   Action = "kvkk.retention_policy.list"
	KvkkRetentionPolicyUpdate Action = "kvkk.retention_policy.update"
	KvkkDestructionLogList    Action = "kvkk.destruction_log.list"
	KvkkBreachList            Action = "kvkk.breach.list"
	KvkkBreachManage          Action = "kvkk.breach.manage"
	KvkkSubprocessorList      Action = "kvkk.subprocessor.list"
	KvkkSubprocessorManage    Action = "kvkk.subprocessor.manage"
)

// System-only actions of the kvkk module (03 §3.7): only SystemActor may perform them.
const (
	KvkkRetentionSweep Action = "kvkk.retention.sweep"
)

// kvkkActions lists the HTTP actions of kvkk in 03 §5.22 order.
var kvkkActions = []Action{
	KvkkDsrCreate,
	KvkkDsrListOwn,
	KvkkDsrReadOwn,
	KvkkDsrCancelOwn,
	KvkkDsrExportDownload,
	KvkkDsrList,
	KvkkDsrRead,
	KvkkDsrCreateAdmin,
	KvkkDsrUpdate,
	KvkkDsrApprove,
	KvkkDsrComplete,
	KvkkDsrReject,
	KvkkRetentionPolicyList,
	KvkkRetentionPolicyUpdate,
	KvkkDestructionLogList,
	KvkkBreachList,
	KvkkBreachManage,
	KvkkSubprocessorList,
	KvkkSubprocessorManage,
}
