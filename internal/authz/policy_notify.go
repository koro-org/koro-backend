package authz

// notifyPolicy is the policy of the notify module (03 §5.19). Only tickets of
// the notify module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every notify action (default deny) until the
// module ticket writes Visible and Grants.
func notifyPolicy() Policy { return Policy{} }
