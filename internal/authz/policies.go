package authz

// modulePolicies is the one-time module → policy dispatch table (BE-001 R5).
// Every constructor lives in policy_<module>.go and is edited only by that
// module's tickets; this table never changes.
func modulePolicies() map[string]Policy {
	return map[string]Policy{
		"identity":     identityPolicy(),
		"orgs":         orgsPolicy(),
		"directory":    directoryPolicy(),
		"students":     studentsPolicy(),
		"consent":      consentPolicy(),
		"events":       eventsPolicy(),
		"feed":         feedPolicy(),
		"outreach":     outreachPolicy(),
		"messaging":    messagingPolicy(),
		"discovery":    discoveryPolicy(),
		"entitlements": entitlementsPolicy(),
		"talent":       talentPolicy(),
		"parties":      partiesPolicy(),
		"catalog":      catalogPolicy(),
		"orders":       ordersPolicy(),
		"payments":     paymentsPolicy(),
		"ledger":       ledgerPolicy(),
		"billing":      billingPolicy(),
		"notify":       notifyPolicy(),
		"files":        filesPolicy(),
		"audit":        auditPolicy(),
		"kvkk":         kvkkPolicy(),
	}
}
