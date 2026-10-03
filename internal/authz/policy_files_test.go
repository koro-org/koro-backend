package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// fileResource is the Resource the files service builds (03 §3.5; H-27):
// OrgID = files.org_id, OwnerUserID = uploaded_by, Attrs purpose.
func fileResource(purpose string, org *uuid.UUID, uploader string) authz.Resource {
	return authz.Resource{Type: authz.TypeFile, OrgID: org, OwnerUserID: ptr(authztest.ID(uploader)),
		Attrs: map[string]string{"purpose": purpose}}
}

func TestFilesPolicy_UploadCreateK30(t *testing.T) {
	t.Parallel()
	clubA, companyA, uniA, platformOrg := ptr(authztest.ID("club_a")), ptr(authztest.ID("company_a")),
		ptr(authztest.ID("uni_a")), ptr(authztest.ID("platform_org"))
	tests := []struct {
		persona string
		purpose string
		org     *uuid.UUID
		want    error
	}{
		{"student", "cv", nil, nil},
		{"student_minor", "avatar", nil, nil},
		{"club_admin", "cv", nil, nil}, // club roles are verified students
		{"user_unverified", "cv", nil, authz.ErrForbidden},
		{"company_admin", "avatar", nil, authz.ErrForbidden},
		{"platform_admin", "cv", nil, authz.ErrForbidden},
		{"user_unverified", "edu_document", nil, nil},
		{"moderator", "edu_document", nil, nil},
		{"club_admin", "org_logo", clubA, nil},
		{"club_member", "org_logo", clubA, authz.ErrForbidden},
		{"other_club_admin", "org_logo", clubA, authz.ErrNotFound},
		{"platform_admin", "org_logo", clubA, nil},
		{"support", "org_logo", clubA, authz.ErrForbidden},
		{"university_rep", "event_cover", uniA, nil},
		{"company_admin", "event_cover", companyA, authz.ErrForbidden}, // k13: companies create no events
		{"platform_admin", "event_cover", clubA, authz.ErrForbidden},   // k-po
		{"company_member", "post_media", companyA, nil},
		{"platform_admin", "post_media", companyA, authz.ErrForbidden},
		{"student", "club_claim_evidence", nil, nil},
		{"student_minor", "club_claim_evidence", nil, authz.ErrForbidden}, // Y4
		{"finance_admin", "party_document", nil, nil},
		{"company_admin", "bank_account_evidence", companyA, nil},
		{"club_member", "party_document", clubA, authz.ErrForbidden},
		{"finance_admin", "transfer_receipt", nil, nil},
		{"platform_admin", "payout_receipt", nil, authz.ErrForbidden},
		{"university_rep", "invoice_pdf", uniA, nil},
		{"company_admin", "invoice_pdf", companyA, authz.ErrForbidden},
		{"finance_admin", "invoice_pdf", nil, nil},
		{"finance_admin", "event_export", nil, authz.ErrForbidden},
		{"student", "dsr_export", nil, authz.ErrForbidden},
		{"student", "selfie", nil, authz.ErrForbidden},
	}
	for _, tt := range tests {
		a := persona(t, tt.persona)
		r := fileResource(tt.purpose, tt.org, tt.persona)
		wantErr(t, authz.Can(t.Context(), a, authz.FilesUploadCreate, r), tt.want, false)
	}

	// k-po: platform_admin may post media for the platform org it belongs to.
	pad := persona(t, "platform_admin")
	pad.Memberships = append(pad.Memberships, authz.Membership{OrgID: *platformOrg, OrgKind: authz.OrgPlatform, Role: authz.MemberStaff})
	wantErr(t, authz.Can(t.Context(), pad, authz.FilesUploadCreate, fileResource("post_media", platformOrg, "platform_admin")), nil, false)

	// k-mfa: an MFA role path without an MFA session.
	ca := persona(t, "company_admin")
	ca.MFAVerified = false
	wantErr(t, authz.Can(t.Context(), ca, authz.FilesUploadCreate, fileResource("org_logo", companyA, "company_admin")), authz.ErrMFARequired, false)

	// k-cs: career_staff is not university_rep.
	cs := member(authz.OrgUniversity, authz.MemberCareerStaff)
	org := authztest.ID("org_university")
	wantErr(t, authz.Can(t.Context(), cs, authz.FilesUploadCreate, fileResource("event_cover", &org, "member_career_staff")), authz.ErrForbidden, false)
}

func TestFilesPolicy_OwnFileOnly(t *testing.T) {
	t.Parallel()
	r := fileResource("cv", nil, "student")
	for _, act := range []authz.Action{authz.FilesUploadComplete, authz.FilesFileDeleteOwn} {
		wantErr(t, authz.Can(t.Context(), persona(t, "student"), act, r), nil, false)
		for _, other := range []string{"student_b", "platform_admin", "support", "dpo"} {
			wantErr(t, authz.Can(t.Context(), persona(t, other), act, r), authz.ErrNotFound, false)
		}
		wantErr(t, authz.Can(t.Context(), authz.Anonymous(), act, r), authz.ErrForbidden, false)
		// A file without an uploader (server-generated) is nobody's to delete here.
		gen := r
		gen.OwnerUserID = nil
		wantErr(t, authz.Can(t.Context(), persona(t, "student"), act, gen), authz.ErrNotFound, false)
	}
	// The purpose Attrs key is mandatory (03 §3.5).
	bare := authz.Resource{Type: authz.TypeFile, OwnerUserID: ptr(authztest.ID("student"))}
	wantErr(t, authz.Can(t.Context(), persona(t, "student"), authz.FilesFileDeleteOwn, bare), authz.ErrForbidden, true)
	// Only the orphan job may purge (03 §3.7).
	wantErr(t, authz.Can(t.Context(), authz.SystemActor("files.orphan_cleanup"), authz.FilesOrphanPurge, authz.Resource{Type: authz.TypeFile}), nil, false)
	wantErr(t, authz.Can(t.Context(), persona(t, "platform_admin"), authz.FilesOrphanPurge, r), authz.ErrForbidden, false)
}
