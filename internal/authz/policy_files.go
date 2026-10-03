package authz

// filesPolicy is the policy of the files module (03 §5.20). Only tickets of
// the files module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every files action (default deny) until the
// module ticket writes Visible and Grants.
func filesPolicy() Policy { return Policy{} }
