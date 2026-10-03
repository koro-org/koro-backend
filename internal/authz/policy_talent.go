package authz

// talentPolicy is the policy of the talent module (03 §5.12). Only tickets of
// the talent module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every talent action (default deny) until the
// module ticket writes Visible and Grants.
func talentPolicy() Policy { return Policy{} }
