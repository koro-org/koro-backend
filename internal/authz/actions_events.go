// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the events module (03 §5.6), in table order.
const (
	EventsEventList             Action = "events.event.list"
	EventsEventRead             Action = "events.event.read"
	EventsEventListOrg          Action = "events.event.list_org"
	EventsEventCreate           Action = "events.event.create"
	EventsEventUpdate           Action = "events.event.update"
	EventsEventPublish          Action = "events.event.publish"
	EventsEventCancel           Action = "events.event.cancel"
	EventsEventDelete           Action = "events.event.delete"
	EventsFormRead              Action = "events.form.read"
	EventsFormManage            Action = "events.form.manage"
	EventsFormPublish           Action = "events.form.publish"
	EventsRegistrationCreate    Action = "events.registration.create"
	EventsRegistrationListOwn   Action = "events.registration.list_own"
	EventsRegistrationReadOwn   Action = "events.registration.read_own"
	EventsRegistrationCancelOwn Action = "events.registration.cancel_own"
	EventsRegistrationList      Action = "events.registration.list"
	EventsRegistrationRead      Action = "events.registration.read"
	EventsRegistrationDecide    Action = "events.registration.decide"
	EventsRegistrationCancel    Action = "events.registration.cancel"
	EventsTicketReadOwn         Action = "events.ticket.read_own"
	EventsTicketResend          Action = "events.ticket.resend"
	EventsTicketReissue         Action = "events.ticket.reissue"
	EventsStaffList             Action = "events.staff.list"
	EventsStaffManage           Action = "events.staff.manage"
	EventsCheckinCreate         Action = "events.checkin.create"
	EventsCheckinVoid           Action = "events.checkin.void"
	EventsCheckinList           Action = "events.checkin.list"
	EventsCheckinStats          Action = "events.checkin.stats"
	EventsAttendanceFinalize    Action = "events.attendance.finalize"
	EventsExportCreate          Action = "events.export.create"
	EventsExportList            Action = "events.export.list"
	EventsExportDownload        Action = "events.export.download"
	EventsLintRuleManage        Action = "events.lint_rule.manage"
)

// System-only actions of the events module (03 §3.7): only SystemActor may perform them.
const (
	EventsRegistrationExpire          Action = "events.registration.expire"
	EventsRegistrationPromoteWaitlist Action = "events.registration.promote_waitlist"
	EventsTicketIssue                 Action = "events.ticket.issue"
	EventsExportPurge                 Action = "events.export.purge"
)

// eventsActions lists the HTTP actions of events in 03 §5.6 order.
var eventsActions = []Action{
	EventsEventList,
	EventsEventRead,
	EventsEventListOrg,
	EventsEventCreate,
	EventsEventUpdate,
	EventsEventPublish,
	EventsEventCancel,
	EventsEventDelete,
	EventsFormRead,
	EventsFormManage,
	EventsFormPublish,
	EventsRegistrationCreate,
	EventsRegistrationListOwn,
	EventsRegistrationReadOwn,
	EventsRegistrationCancelOwn,
	EventsRegistrationList,
	EventsRegistrationRead,
	EventsRegistrationDecide,
	EventsRegistrationCancel,
	EventsTicketReadOwn,
	EventsTicketResend,
	EventsTicketReissue,
	EventsStaffList,
	EventsStaffManage,
	EventsCheckinCreate,
	EventsCheckinVoid,
	EventsCheckinList,
	EventsCheckinStats,
	EventsAttendanceFinalize,
	EventsExportCreate,
	EventsExportList,
	EventsExportDownload,
	EventsLintRuleManage,
}
