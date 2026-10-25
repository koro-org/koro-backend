package feed

import "testing"

type auditLog struct{ events []string }

func wantErr(t *testing.T, err, want error) {}

// parseID in a test file is not banned (the production list does not apply).
func parseID(s string) string { return s }
