package authz

// studentsPolicy is the policy of the students module (03 §5.4). Only tickets of
// the students module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every students action (default deny) until the
// module ticket writes Visible and Grants.
func studentsPolicy() Policy { return Policy{} }
