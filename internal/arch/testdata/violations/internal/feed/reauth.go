// A function-typed reauth port (TestArch_ReauthFailureAudited). Parsed,
// never compiled.
package feed

import "context"

// Pin returns the refusal of its function port as it came.
func (s *service) Pin(ctx context.Context, a actor, proof reauthProof) error {
	if _, err := s.d.Reauth(ctx, a.UserID, proof); err != nil {
		return err
	}
	return nil
}
