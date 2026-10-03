package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/directory"
)

// matrixFixtures seeds the directory rows of the matrix (03 §7.3): a
// universities row for every university org of authztest (ID
// authztest.UniversityID(org): "uni_a_university"), a clubs row for every
// club org (club_sensitive is a sensitive religious club; club_a and club_b
// are claimed), and the fixtures of fixtures/directory.yaml (an inactive
// university, a faculty, a department, a skill; BE-022: claim_pending,
// student's pending claim for club_unclaimed). Every row is dated
// matrixOrgTime, so an untouched club's ETag is "1hmhuo3hji000" (the
// If-Match the matrix files send).
func (w *directoryWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	st := w.built
	t := matrixOrgTime
	city := map[string]string{"uni_a": "İstanbul", "uni_b": "Ankara"}
	for _, o := range authztest.Orgs() {
		if o.Kind != authz.OrgUniversity {
			continue
		}
		if _, err := st.InsertUniversity(ctx, directory.University{
			ID: authztest.UniversityID(o.Name), OrgID: authztest.ID(o.Name), YokCode: "MATRIS-" + o.Name,
			Name: "Matris " + o.Name, City: city[o.Name], Type: directory.UniversityState, IsActive: true,
			CreatedAt: t, UpdatedAt: t,
		}); err != nil {
			return fmt.Errorf("university %s: %w", o.Name, err)
		}
	}
	for _, o := range authztest.Orgs() {
		if o.Kind != authz.OrgClub {
			continue
		}
		c := directory.Club{
			OrgID: authztest.ID(o.Name), UniversityID: authztest.UniversityID(o.University),
			Category: directory.CategoryTechnology, JoinPolicy: directory.JoinApproval, ClaimStatus: directory.ClaimUnclaimed,
			CreatedAt: t, UpdatedAt: t,
		}
		if o.Claimed {
			c.ClaimStatus = directory.ClaimClaimed
		}
		if o.Sensitive {
			reason, by := directory.SensitiveReason("religion"), authztest.ID("platform_admin")
			c.Category, c.IsSensitive, c.SensitiveReason, c.SensitivitySetBy, c.SensitivitySetAt =
				directory.CategoryReligious, true, &reason, &by, &t
		}
		if _, err := st.InsertClub(ctx, c); err != nil {
			return fmt.Errorf("club %s: %w", o.Name, err)
		}
	}
	// Parents first: universities, faculties, departments, then skills.
	order := map[string]int{"university": 0, "faculty": 1, "department": 2, "skill": 3, "claim": 4}
	fixtures := fx.Of("directory")
	slices.SortStableFunc(fixtures, func(a, b fixture) int { return order[a.Kind] - order[b.Kind] })
	for _, f := range fixtures {
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		switch f.Kind {
		case "university":
			active, _ := f.Fields["active"].(bool)
			_, err = st.InsertUniversity(ctx, directory.University{
				ID: id, OrgID: authztest.ID(f.Name + "_org"), YokCode: "MATRIS-" + f.Name, Name: "Matris " + f.Name,
				City: "İzmir", Type: directory.UniversityState, IsActive: active, CreatedAt: t, UpdatedAt: t,
			})
		case "faculty":
			uni, rerr := fx.Ref(f, "university")
			if rerr != nil {
				return rerr
			}
			_, err = st.InsertFaculty(ctx, directory.Faculty{ID: id, UniversityID: uni, Name: "Matris Fakültesi", IsActive: true})
		case "department":
			uni, rerr := fx.Ref(f, "university")
			if rerr != nil {
				return rerr
			}
			fac, rerr := fx.Ref(f, "faculty")
			if rerr != nil {
				return rerr
			}
			_, err = st.InsertDepartment(ctx, directory.Department{
				ID: id, UniversityID: uni, FacultyID: &fac, Name: "Matris Mühendisliği", DegreeLevel: directory.DegreeBachelor,
				IsActive: true,
			})
		case "skill":
			_, err = st.InsertSkill(ctx, directory.Skill{
				ID: id, Slug: "matris-" + strings.ReplaceAll(f.Name, "_", "-"), NameTR: "Matris yetkinliği", Category: directory.SkillCategory("technical"),
				IsActive: true,
			})
		case "claim":
			claimant, rerr := fx.Ref(f, "claimant")
			if rerr != nil {
				return rerr
			}
			club, rerr := fx.Ref(f, "club")
			if rerr != nil {
				return rerr
			}
			text := "Genel kurul tutanağı kulüp danışmanındadır."
			_, err = st.InsertClaim(ctx, directory.ClubClaim{
				ID: id, ClubOrgID: club, ClaimantUserID: claimant, Status: directory.ClaimPending, RoleInClub: "Başkan",
				EvidenceText: &text, CreatedAt: t, UpdatedAt: t,
			})
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
		if err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
	}
	return nil
}
