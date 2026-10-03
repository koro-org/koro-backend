package authz

// eventsPolicy is the policy of the events module (03 §5.6). Only tickets of
// the events module edit this file (BE-001 R5).
//
// Stub: the zero Policy denies every events action (default deny) until the
// module ticket writes Visible and Grants.
func eventsPolicy() Policy { return Policy{} }
