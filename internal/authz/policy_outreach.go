package authz

// outreachPolicy is the policy of the outreach module (03 §5.8). Only tickets of
// the outreach module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every outreach action (default deny) until the
// module ticket writes Visible and Grants.
func outreachPolicy() Policy { return Policy{} }
