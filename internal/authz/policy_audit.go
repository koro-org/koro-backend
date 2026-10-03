package authz

// auditPolicy is the policy of the audit module (03 §5.21). Only tickets of
// the audit module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every audit action (default deny) until the
// module ticket writes Visible and Grants.
func auditPolicy() Policy { return Policy{} }
