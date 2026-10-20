// The miniature identity module (TestArch_ReauthFailureAudited). Parsed,
// never compiled.
package identity

import (
	"context"
	"uuid"
)

// Verify is the reauth port: its caller audits (reauthDirect).
func (s *service) Verify(ctx context.Context, userID uuid.UUID, proof ReauthProof) (string, error) {
	return s.verify(ctx, userID, proof, false)
}
