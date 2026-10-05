package directory

import (
	"context"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/privacy"
)

// eraseJob is the system actor of the erasure's deletions; eraseAction is
// their audit action (a system record, not an HTTP action of 03 §5.3).
const (
	eraseJob    = "directory.erase_account"
	eraseAction = authz.Action("directory.club_claim.erase")
)

// privacyProvider is the KVKK provider of directory (01 §3.3). The
// reference data and the club catalog hold no personal data (02 §8.1-8.5,
// 8.7: set_by/verified_by reference staff accounts). What directory knows
// about a person is their club claims (02 §8.6) and the evidence files
// attached to them.
//
// Export returns every claim of the data subject, the decision note
// included (it is a record about them; the app shows it to the reviewers only).
// Erase hard deletes the claims (02 §8.6 "hard delete") and then their
// evidence files (files.Files.Delete through Deps.Files). The role an
// approved claim granted is ended by the orgs provider (account_erased).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "directory" }

// claimsOf reads every claim of a user (page by page).
func (s *service) claimsOf(ctx context.Context, userID uuid.UUID) ([]ClubClaim, error) {
	var out []ClubClaim
	var after *uuid.UUID
	for {
		rows, err := s.d.Store.ClaimsOf(ctx, userID, after, 1000)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < 1000 {
			return out, nil
		}
		after = new(rows[len(rows)-1].ID)
	}
}

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	cs, err := p.s.claimsOf(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "directory", Items: make([]map[string]any, 0, len(cs))}
	for _, c := range cs {
		item := map[string]any{
			"record": "club_claim", "claim_id": c.ID.String(), "club_org_id": c.ClubOrgID.String(),
			"status": string(c.Status), "role_in_club": c.RoleInClub, "created_at": c.CreatedAt.UTC().Format(time.RFC3339),
		}
		if c.EvidenceText != nil {
			item["evidence_text"] = *c.EvidenceText
		}
		if c.EvidenceFileID != nil {
			item["evidence_file_id"] = c.EvidenceFileID.String()
		}
		if c.ReviewedAt != nil {
			item["decided_at"] = c.ReviewedAt.UTC().Format(time.RFC3339)
		}
		if c.DecisionNote != nil {
			item["decision_note"] = *c.DecisionNote
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Erase implements privacy.Provider. It is idempotent: a second run finds
// no claim. The claims go in one transaction (with an audit record each);
// their files are deleted after the reference is gone.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	s := p.s
	a := authz.SystemActor(eraseJob)
	res := privacy.EraseResult{Module: "directory"}
	var fileIDs []uuid.UUID
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cs, err := s.claimsOf(ctx, userID)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if err := s.d.Store.DeleteClaim(ctx, c.ID); err != nil {
				return err
			}
			if c.EvidenceFileID != nil {
				fileIDs = append(fileIDs, *c.EvidenceFileID)
			}
			ev := auditEvent(a, eraseAction, authz.TypeClubClaim, c.ID, nil, "",
				map[string]any{"operation": "erase", audit.MetaSubjectUserID: userID.String(), authz.AttrStatus: string(c.Status)})
			if err := s.d.Audit.Log(ctx, ev); err != nil {
				return err
			}
			res.Deleted++
		}
		return nil
	})
	if err != nil {
		return privacy.EraseResult{}, err
	}
	for _, f := range fileIDs {
		if err := s.removeFile(ctx, a, f); err != nil {
			return res, err
		}
	}
	return res, nil
}

// removeFile deletes an evidence file through Deps.Files (nil: the file
// stays, logged without personal data).
func (s *service) removeFile(ctx context.Context, a authz.Actor, fileID uuid.UUID) error {
	if s.d.Files == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "claim evidence not removed: files not wired",
			slog.String("file_id", fileID.String()))
		return nil
	}
	return s.d.Files.Delete(ctx, a, fileID)
}
