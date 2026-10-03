package authz

// ordersPolicy is the policy of the orders module (03 §5.15). Only tickets of
// the orders module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every orders action (default deny) until the
// module ticket writes Visible and Grants.
func ordersPolicy() Policy { return Policy{} }
