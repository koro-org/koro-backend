package files

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/privacy"
)

// privacyProvider is the KVKK provider of files (01 §3.3): the user's own
// files that no module has attached (abandoned or pending uploads). Attached
// files are exported and erased by their owning modules, which call
// files.Delete.
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "files" }

// Export implements privacy.Provider: metadata only (the content is
// downloadable through the owning features).
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	rows, err := p.s.d.Store.UnattachedOf(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "files", Items: make([]map[string]any, 0, len(rows))}
	for _, f := range rows {
		m := f.meta()
		out.Items = append(out.Items, map[string]any{
			"file_id": m.ID.String(), "purpose": string(m.Purpose), "name": m.OriginalName, "mime": m.MIME,
			"size_bytes": m.SizeBytes, "status": string(m.Status), "created_at": m.CreatedAt.Format(time.RFC3339),
		})
	}
	return out, nil
}

// privacyEraseJob is the system actor of the deletions of an erasure.
const privacyEraseJob = "files.privacy_erase"

// Erase implements privacy.Provider: the unattached files are hard deleted
// with their objects (audited as files.file.delete by privacyEraseJob). It
// is idempotent.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	rows, err := p.s.d.Store.UnattachedOf(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	res := privacy.EraseResult{Module: "files"}
	for _, f := range rows {
		if err := p.s.remove(ctx, authz.SystemActor(privacyEraseJob), auditDelete, f.ID, nil); err != nil {
			return res, err
		}
		res.Deleted++
	}
	return res, nil
}
