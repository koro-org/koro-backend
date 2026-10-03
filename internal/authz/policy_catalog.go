package authz

// catalogPolicy is the policy of the catalog module (03 §5.14). Only tickets of
// the catalog module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every catalog action (default deny) until the
// module ticket writes Visible and Grants.
func catalogPolicy() Policy { return Policy{} }
