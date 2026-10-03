// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the discovery module (03 §5.10), in table order.
const (
	DiscoveryProfileSearch Action = "discovery.profile.search"
	DiscoveryProfileRead   Action = "discovery.profile.read"
)

// discoveryActions lists the HTTP actions of discovery in 03 §5.10 order.
var discoveryActions = []Action{
	DiscoveryProfileSearch,
	DiscoveryProfileRead,
}
