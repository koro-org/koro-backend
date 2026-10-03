package authz

// kvkkPolicy is the policy of the kvkk module (03 §5.22). Only tickets of
// the kvkk module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every kvkk action (default deny) until the
// module ticket writes Visible and Grants.
func kvkkPolicy() Policy { return Policy{} }
