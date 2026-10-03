package authz

// entitlementsPolicy is the policy of the entitlements module (03 §5.11). Only tickets of
// the entitlements module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every entitlements action (default deny) until the
// module ticket writes Visible and Grants.
func entitlementsPolicy() Policy { return Policy{} }
