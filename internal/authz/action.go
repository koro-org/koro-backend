package authz

import (
	"slices"
	"strings"
)

// Action is "<module>.<entity>.<verb>" (03 §4.1). The audit_events.action
// of an operation is the same string (02 §2.1).
type Action string

// Module returns the module part of the action ("events" for
// "events.registration.create").
func (a Action) Module() string {
	m, _, _ := strings.Cut(string(a), ".")
	return m
}

// String returns the action name.
func (a Action) String() string { return string(a) }

// Modules returns the 22 feature modules in 03 §5 table order; every one has
// an actions_<module>.go and a policy_<module>.go.
func Modules() []string {
	return []string{
		"identity", "orgs", "directory", "students", "consent", "events", "feed", "outreach",
		"messaging", "discovery", "entitlements", "talent", "parties", "catalog", "orders",
		"payments", "ledger", "billing", "notify", "files", "audit", "kvkk",
	}
}

// httpActionsByModule is the one-time module → HTTP action table (R5), in
// the order of Modules().
var httpActionsByModule = map[string][]Action{
	"identity":     identityActions,
	"orgs":         orgsActions,
	"directory":    directoryActions,
	"students":     studentsActions,
	"consent":      consentActions,
	"events":       eventsActions,
	"feed":         feedActions,
	"outreach":     outreachActions,
	"messaging":    messagingActions,
	"discovery":    discoveryActions,
	"entitlements": entitlementsActions,
	"talent":       talentActions,
	"parties":      partiesActions,
	"catalog":      catalogActions,
	"orders":       ordersActions,
	"payments":     paymentsActions,
	"ledger":       ledgerActions,
	"billing":      billingActions,
	"notify":       notifyActions,
	"files":        filesActions,
	"audit":        auditActions,
	"kvkk":         kvkkActions,
}

// systemActions is the 03 §3.7 allow-list of SystemActor, in document order.
// events.attendance.finalize is also an HTTP action (03 §5.6).
var systemActions = []Action{
	EventsRegistrationExpire,
	EventsRegistrationPromoteWaitlist,
	EventsAttendanceFinalize,
	EventsTicketIssue,
	EventsExportPurge,
	OrdersOrderExpire,
	OrdersLineAutoFulfill,
	EntitlementsEntitlementExpire,
	MessagingContactRequestExpire,
	OutreachDispatchRun,
	FeedOrgSettingsAutoTrust,
	NotifyMessageSend,
	TalentLedgerWrite,
	TalentScoreCompute,
	KvkkRetentionSweep,
	IdentityEduVerificationExpire,
	OrgsMemberEndRole,
	BillingInvoicePlan,
	AuditChainAppend,
	FilesOrphanPurge,
	WebhookInboxProcess,
}

// HTTPActions returns the 289 actions of the 03 §5 tables (module order,
// then table order). Every one must be bound to at least one operation
// (03 §7.4-5).
func HTTPActions() []Action {
	var out []Action
	for _, m := range Modules() {
		out = append(out, httpActionsByModule[m]...)
	}
	return out
}

// ModuleActions returns the HTTP actions of one module (nil if unknown).
func ModuleActions(module string) []Action { return slices.Clone(httpActionsByModule[module]) }

// SystemActions returns the 21 actions SystemActor may perform (03 §3.7).
func SystemActions() []Action { return slices.Clone(systemActions) }

// Actions returns every known action (HTTP and system, each once), sorted.
func Actions() []Action {
	out := append(HTTPActions(), systemActions...)
	slices.Sort(out)
	return slices.Compact(out)
}

// KnownAction reports whether name is a declared action. Unknown actions are
// always denied (03 §3.2-0).
func KnownAction(name string) bool { return std.known(Action(name)) }

// IsHTTPAction reports whether a belongs to a 03 §5 table.
func IsHTTPAction(a Action) bool { return std.http[a] }

// IsSystemAction reports whether a is in the 03 §3.7 allow-list.
func IsSystemAction(a Action) bool { return std.system[a] }
