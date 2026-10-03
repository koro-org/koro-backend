package authz

// orgsPolicy is the policy of the orgs module (03 §5.2). Only tickets of
// the orgs module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every orgs action (default deny) until the
// module ticket writes Visible and Grants.
func orgsPolicy() Policy { return Policy{} }
