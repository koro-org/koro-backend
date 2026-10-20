// An R operation without a proof check through the reauth port
// (TestArch_ReauthFailureAudited). Parsed, never compiled.
package students

import (
	"context"

	"koro/internal/platform/httpx/op"
)

var cvExportMeta = op.Meta{Action: "students.cv.export", Reauth: true}

// ExportCV checks its proof beside the port: no denied record either.
func (s *service) ExportCV(ctx context.Context, a actor, proof reauthProof) error {
	return s.d.CheckPassword(ctx, a.UserID, proof)
}
