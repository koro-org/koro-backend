// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the outreach module (03 §5.8), in table order.
const (
	OutreachSegmentList    Action = "outreach.segment.list"
	OutreachSegmentRead    Action = "outreach.segment.read"
	OutreachSegmentManage  Action = "outreach.segment.manage"
	OutreachAudienceCount  Action = "outreach.audience.count"
	OutreachDispatchCreate Action = "outreach.dispatch.create"
	OutreachDispatchList   Action = "outreach.dispatch.list"
	OutreachDispatchRead   Action = "outreach.dispatch.read"
	OutreachDispatchCancel Action = "outreach.dispatch.cancel"
)

// System-only actions of the outreach module (03 §3.7): only SystemActor may perform them.
const (
	OutreachDispatchRun Action = "outreach.dispatch.run"
)

// outreachActions lists the HTTP actions of outreach in 03 §5.8 order.
var outreachActions = []Action{
	OutreachSegmentList,
	OutreachSegmentRead,
	OutreachSegmentManage,
	OutreachAudienceCount,
	OutreachDispatchCreate,
	OutreachDispatchList,
	OutreachDispatchRead,
	OutreachDispatchCancel,
}
