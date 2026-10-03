package authz

// messagingPolicy is the policy of the messaging module (03 §5.9). Only tickets of
// the messaging module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every messaging action (default deny) until the
// module ticket writes Visible and Grants.
func messagingPolicy() Policy { return Policy{} }
