package students

import (
	"context"
	"errors"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/privacy"
)

// eraseJob is the system actor of the erasure (avatar deletion audit).
const eraseJob = "students.erase_account"

// privacyProvider is the KVKK provider of students (01 §3.3): the profile
// (with its derived audience columns), the skills, experiences and
// languages, the privacy settings, the CVs, the avatar and CV files and
// the level 3 access grants are the student's personal data (02 §9.1–9.7:
// hard delete with the account, the files included). The consent rows behind the mirrored flags belong
// to consent's provider; the CV files' bytes are files' (the export lists
// them).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "students" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	st := p.s.d.Store
	out := privacy.ExportSection{Module: "students", Items: []map[string]any{}}
	switch pr, err := st.GetProfile(ctx, userID); {
	case err == nil:
		out.Items = append(out.Items, exportProfile(pr))
	case !errors.Is(err, ErrNotFound):
		return privacy.ExportSection{}, err
	}
	switch pv, err := st.GetPrivacy(ctx, userID); {
	case err == nil:
		out.Items = append(out.Items, map[string]any{
			"record": "privacy_settings", "discoverable": pv.Discoverable, "show_contact_on_grant": pv.ShowContactOnGrant,
			"show_clubs_on_grant": pv.ShowClubsOnGrant, "activity_ranking_choice": string(pv.ActivityRankingChoice),
			"updated_at": pv.UpdatedAt.UTC().Format(time.RFC3339),
		})
	case !errors.Is(err, ErrNotFound):
		return privacy.ExportSection{}, err
	}
	skills, err := st.Skills(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, sk := range skills {
		item := map[string]any{"record": "skill", "skill_id": sk.SkillID.String()}
		if sk.Level != nil {
			item["level"] = int(*sk.Level)
		}
		out.Items = append(out.Items, item)
	}
	exps, err := st.Experiences(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, e := range exps {
		item := map[string]any{
			"record": "experience", "experience_id": e.ID.String(), "kind": string(e.Kind), "title": e.Title,
			"start_month": monthString(e.StartMonth),
		}
		putStr(item, "organization_name", e.OrganizationName)
		putStr(item, "description", e.Description)
		if e.EndMonth != nil {
			item["end_month"] = monthString(*e.EndMonth)
		}
		out.Items = append(out.Items, item)
	}
	langs, err := st.Languages(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, l := range langs {
		out.Items = append(out.Items, map[string]any{"record": "language", "language_code": l.LanguageCode, "level": string(l.Level)})
	}
	cvs, err := st.CVs(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	views, err := p.s.cvViews(ctx, cvs)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, c := range views {
		out.Items = append(out.Items, map[string]any{
			"record": "cv", "cv_id": c.CV.ID.String(), "file_id": c.CV.FileID.String(), "file_name": c.FileName,
			"is_current": c.CV.IsCurrent, "created_at": c.CV.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	grants, err := st.AllGrants(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, g := range grants {
		out.Items = append(out.Items, exportGrant(g))
	}
	return out, nil
}

// exportGrant is a level 3 grant in the export: which company had access,
// from which request, its state, when a lapsed verification suspended it
// (BE-077), when the student resumed it (BE-088) and until when, who ended
// it and why. The revocation reason is exported whoever wrote it (user
// decision Q-08, T-338, KVKK md. 11 right of access): the student's own,
// the company's note and the system's (suspension_expired). It is in the
// export only: no answer of the app shows a reason to the other side.
func exportGrant(g AccessGrant) map[string]any {
	item := map[string]any{
		"record": "access_grant", "grant_id": g.ID.String(), "company_org_id": g.CompanyOrgID.String(),
		"source": string(g.Source), "source_id": g.SourceID.String(), "granted_at": g.GrantedAt.UTC().Format(time.RFC3339),
		"status": string(g.Status()),
	}
	if g.SuspendedAt != nil {
		item["suspended_at"] = g.SuspendedAt.UTC().Format(time.RFC3339)
	}
	if g.ResumedAt != nil {
		item["resumed_at"] = g.ResumedAt.UTC().Format(time.RFC3339)
	}
	if g.ContactSharedAt != nil { // the e-mail shared with this company (BE-089)
		item["contact_shared_at"] = g.ContactSharedAt.UTC().Format(time.RFC3339)
	}
	if g.RevokedAt != nil {
		item["revoked_at"] = g.RevokedAt.UTC().Format(time.RFC3339)
	}
	if g.RevokedByKind != nil {
		item["revoked_by"] = string(*g.RevokedByKind)
	}
	putStr(item, "revoke_reason", g.RevokeReason)
	return item
}

func exportProfile(p Profile) map[string]any {
	item := map[string]any{
		"record": "student_profile", "university_id": p.UniversityID.String(), "degree_level": string(p.DegreeLevel),
		"status": string(p.Status), "completed_sections": []string(p.CompletedSections),
		"audience_keys": []string(p.AudienceKeys), "audience_eligible": p.AudienceEligible,
		"created_at": p.CreatedAt.UTC().Format(time.RFC3339), "updated_at": p.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if p.DepartmentID != nil {
		item["department_id"] = p.DepartmentID.String()
	}
	if p.ClassYear != nil {
		item["class_year"] = int(*p.ClassYear)
	}
	if p.ExpectedGraduation != nil {
		item["expected_graduation"] = monthString(*p.ExpectedGraduation)
	}
	if p.GraduatedAt != nil {
		item["graduated_at"] = p.GraduatedAt.UTC().Format(time.RFC3339)
	}
	putStr(item, "headline", p.Headline)
	putStr(item, "about", p.About)
	putStr(item, "city", p.City)
	if p.AvatarFileID != nil {
		item["avatar_file_id"] = p.AvatarFileID.String()
	}
	return item
}

func putStr(item map[string]any, key string, v *string) {
	if v != nil {
		item[key] = *v
	}
}

// Erase implements privacy.Provider: in one transaction the sections, the
// access grants, the CVs, the settings and the profile are hard deleted, then the avatar and
// the CV files (no longer referenced) are deleted through files.
// Idempotent: a second run finds nothing.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	s := p.s
	res := privacy.EraseResult{Module: "students"}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var avatar *uuid.UUID
		switch pr, err := s.d.Store.LockProfile(ctx, userID); {
		case err == nil:
			avatar = pr.AvatarFileID
		case !errors.Is(err, ErrNotFound):
			return err
		}
		n, err := s.d.Store.DeleteSections(ctx, userID)
		if err != nil {
			return err
		}
		res.Deleted = n
		// Level 3 grants go with the account: no company keeps access.
		if n, err = s.d.Store.DeleteGrants(ctx, userID); err != nil {
			return err
		}
		res.Deleted += n
		cvs, err := s.d.Store.CVs(ctx, userID)
		if err != nil {
			return err
		}
		for _, c := range cvs {
			if _, err := s.d.Store.DeleteCV(ctx, userID, c.ID); err != nil {
				return err
			}
			res.Deleted++
		}
		for _, del := range []func(context.Context, uuid.UUID) (bool, error){s.d.Store.DeletePrivacy, s.d.Store.DeleteProfile} {
			ok, err := del(ctx, userID)
			if err != nil {
				return err
			}
			if ok {
				res.Deleted++
			}
		}
		if avatar != nil {
			if err := s.removeFile(ctx, authz.SystemActor(eraseJob), *avatar); err != nil {
				return err
			}
			res.Deleted++
		}
		for _, c := range cvs {
			if err := s.removeFile(ctx, authz.SystemActor(eraseJob), c.FileID); err != nil {
				return err
			}
			res.Deleted++
		}
		return nil
	})
	if err != nil {
		return privacy.EraseResult{Module: "students"}, err
	}
	return res, nil
}
