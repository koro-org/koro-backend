package authz

// discoveryPolicy is the policy of the discovery module (03 §5.10). Only tickets of
// the discovery module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every discovery action (default deny) until the
// module ticket writes Visible and Grants.
func discoveryPolicy() Policy { return Policy{} }
