package authz

// ledgerPolicy is the policy of the ledger module (03 §5.17). Only tickets of
// the ledger module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every ledger action (default deny) until the
// module ticket writes Visible and Grants.
func ledgerPolicy() Policy { return Policy{} }
