package authz

// paymentsPolicy is the policy of the payments module (03 §5.16). Only tickets of
// the payments module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every payments action (default deny) until the
// module ticket writes Visible and Grants.
func paymentsPolicy() Policy { return Policy{} }
