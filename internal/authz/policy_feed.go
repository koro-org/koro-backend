package authz

// feedPolicy is the policy of the feed module (03 §5.7). Only tickets of
// the feed module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every feed action (default deny) until the
// module ticket writes Visible and Grants.
func feedPolicy() Policy { return Policy{} }
