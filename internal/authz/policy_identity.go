package authz

// identityPolicy is the policy of the identity module (03 §5.1). Only tickets of
// the identity module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every identity action (default deny) until the
// module ticket writes Visible and Grants.
func identityPolicy() Policy { return Policy{} }
