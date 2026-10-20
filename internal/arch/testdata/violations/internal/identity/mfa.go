// identity's own R operations (TestArch_ReauthFailureAudited). Parsed,
// never compiled.
package identity

import (
	"context"

	aud "koro/internal/audit"
	"koro/internal/platform/httpx/op"
)

var disableMeta = op.Meta{Action: "identity.mfa.disable", Reauth: true}

// DisableTOTP is the shape before BE-075: identity's own R operation
// returns the refusal as it came.
func (s *service) DisableTOTP(ctx context.Context, a actor, proof ReauthProof) error {
	kind, err := s.verify(ctx, a.UserID, proof, false)
	if err != nil {
		return err
	}
	return s.disable(ctx, kind)
}

// reauth routes the refusal through an aliased audit import: fine.
func (s *service) reauth(ctx context.Context, a actor, proof ReauthProof, denied aud.Event) (string, error) {
	kind, err := s.verify(ctx, a.UserID, proof, false)
	if err != nil {
		return "", aud.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied.By(a), err)
	}
	return kind, nil
}
