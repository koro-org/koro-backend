package authz

// directoryPolicy is the policy of the directory module (03 §5.3). Only tickets of
// the directory module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every directory action (default deny) until the
// module ticket writes Visible and Grants.
func directoryPolicy() Policy { return Policy{} }
