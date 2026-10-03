package authz

// partiesPolicy is the policy of the parties module (03 §5.13). Only tickets of
// the parties module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every parties action (default deny) until the
// module ticket writes Visible and Grants.
func partiesPolicy() Policy { return Policy{} }
