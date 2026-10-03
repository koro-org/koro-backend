package authz

// billingPolicy is the policy of the billing module (03 §5.18). Only tickets of
// the billing module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every billing action (default deny) until the
// module ticket writes Visible and Grants.
func billingPolicy() Policy { return Policy{} }
