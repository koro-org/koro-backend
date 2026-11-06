package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/orgs"
)

// Org reports keep what was reported (BE-180, 00 T-401; the rules of BE-160):
// the report carries a copy of the org's profile (OrgSnapshot), and a logo
// the org replaces while a report on it is open or in review is kept for the
// moderators (org_dropped_logos) instead of deleted (BE-155). An edit adds no
// copy: every report keeps the version its reporter saw, and a moderator sees
// the current profile beside it.

// auditLogoKept is the audit action of a logo kept for the moderators (a
// module call of updateOrg, 03 §4.1 format; no HTTP action). It has no org
// scope, like the report itself: the org's own trail must not say it was
// reported.
const auditLogoKept = "feed.org.keep_replaced_logo"

// OrgProfileView is a reported org's profile now, for the moderators.
type OrgProfileView struct {
	orgs.Profile
	SocialLinks directory.SocialLinks // a club's
	LogoURL     string                // 5 minutes, while the scan clears it
}

// DroppedLogoView is a logo kept for the moderators.
type DroppedLogoView struct {
	DroppedLogo
	URL string
}

// LogoKeeper is the moderators' claim on a logo the org replaced (BE-180):
// orgs (L5, below feed) asks through its own port, bound in cmd/koro (01 §6.3
// "Şikâyet edilen kuruluşun logosu"), because the rule depends on the reports.
type LogoKeeper interface {
	// KeepReplacedLogo keeps fileID, which org orgID has just stopped naming,
	// in the caller's transaction when a report on the org is open or in
	// review, and reports whether it did; otherwise the caller deletes the
	// file. A kept file is retired: no public copy, not attachable again, to
	// the org it is gone. Without the files module nothing can be kept.
	KeepReplacedLogo(ctx context.Context, a authz.Actor, orgID, fileID uuid.UUID) (kept bool, err error)
}

// KeepReplacedLogo implements LogoKeeper. updateOrg holds the org's row, and
// a report takes a share lock on it before it is written (takeOrgSnapshot),
// so a report in flight is seen here, or sees the new logo.
func (s *service) KeepReplacedLogo(ctx context.Context, a authz.Actor, orgID, fileID uuid.UUID) (bool, error) {
	if s.d.Files == nil {
		return false, nil
	}
	open, err := s.reports().OpenReporters(ctx, TargetOrg, orgID)
	if err != nil || open == 0 {
		return false, err
	}
	if err := s.logos().DropLogo(ctx, DroppedLogo{FileID: fileID, OrgID: orgID, DroppedAt: s.now()}); err != nil {
		return false, err
	}
	if err := s.d.Files.Retire(ctx, a, fileID); err != nil {
		return false, err
	}
	id := fileID
	return true, s.d.Audit.Log(ctx, audit.Event{
		Action: auditLogoKept, Outcome: audit.Success, ResourceType: authz.TypeFile, ResourceID: &id,
		Metadata: map[string]any{"org_id": orgID.String(), "open_reports": open},
	}.By(a))
}

// takeOrgSnapshot writes the reported org's profile into r, in the report's
// transaction.
func (s *service) takeOrgSnapshot(ctx context.Context, r *Report) error {
	p, err := s.d.Orgs.ReportedProfile(ctx, r.TargetID)
	if err != nil {
		return err
	}
	links, err := s.clubLinks(ctx, p.Kind, r.TargetID)
	if err != nil {
		return err
	}
	var linksJSON JSON
	if len(links) > 0 {
		if linksJSON, err = json.Marshal(links); err != nil {
			return fmt.Errorf("feed: org snapshot: %w", err)
		}
	}
	name, at := p.Name, s.now()
	r.OrgSnapshot = OrgSnapshot{SnapshotOrgName: &name, SnapshotOrgDescription: p.Description, SnapshotOrgWebsiteURL: p.WebsiteURL,
		SnapshotOrgSocialLinks: linksJSON, SnapshotOrgLogoFileID: p.LogoFileID}
	r.SnapshotAt = &at
	return nil
}

// clubLinks returns the social links of a club org; none for another kind,
// a club directory does not know, or without directory.
func (s *service) clubLinks(ctx context.Context, kind orgs.Kind, orgID uuid.UUID) (directory.SocialLinks, error) {
	if kind != orgs.KindClub || s.d.Clubs == nil {
		return nil, nil
	}
	c, err := s.d.Clubs.GetClub(ctx, orgID)
	switch {
	case errors.Is(err, directory.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return c.SocialLinks, nil
}

// reportedOrg adds what a moderator weighs of a reported org to t: its
// profile now, the logos kept for the moderators and the URL of the logo r's
// snapshot names (a file gone since, or one the scan has not cleared, has
// none).
func (s *service) reportedOrg(ctx context.Context, t *TargetSummary, r Report) error {
	p, err := s.d.Orgs.ReportedProfile(ctx, t.Org.ID)
	if err != nil {
		return err
	}
	links, err := s.clubLinks(ctx, p.Kind, t.Org.ID)
	if err != nil {
		return err
	}
	t.Profile = &OrgProfileView{Profile: p, SocialLinks: links}
	if p.LogoFileID != nil {
		t.Profile.LogoURL = s.mediaURL(ctx, *p.LogoFileID)
	}
	logos, err := s.logos().DroppedLogosOf(ctx, t.Org.ID)
	if err != nil {
		return err
	}
	for _, l := range logos {
		t.DroppedLogos = append(t.DroppedLogos, DroppedLogoView{DroppedLogo: l, URL: s.mediaURL(ctx, l.FileID)})
	}
	if r.SnapshotOrgLogoFileID != nil {
		t.SnapshotLogoURL = s.mediaURL(ctx, *r.SnapshotOrgLogoFileID)
	}
	return nil
}
