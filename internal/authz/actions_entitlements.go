// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the entitlements module (03 §5.11), in table order.
const (
	EntitlementsEntitlementList   Action = "entitlements.entitlement.list"
	EntitlementsEntitlementRead   Action = "entitlements.entitlement.read"
	EntitlementsUsageList         Action = "entitlements.usage.list"
	EntitlementsEntitlementGrant  Action = "entitlements.entitlement.grant"
	EntitlementsEntitlementRevoke Action = "entitlements.entitlement.revoke"
)

// System-only actions of the entitlements module (03 §3.7): only SystemActor may perform them.
const (
	EntitlementsEntitlementExpire Action = "entitlements.entitlement.expire"
)

// entitlementsActions lists the HTTP actions of entitlements in 03 §5.11 order.
var entitlementsActions = []Action{
	EntitlementsEntitlementList,
	EntitlementsEntitlementRead,
	EntitlementsUsageList,
	EntitlementsEntitlementGrant,
	EntitlementsEntitlementRevoke,
}
