package students

import (
	"context"
	"encoding/hex"
	"net/http"
	"strconv"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/civil"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
)

// Register adds the students operations of BE-023, BE-025 and BE-046
// (04 §2.4) to
// api. svc may be nil (`koro openapi`): handlers only touch it when
// called. Every answer carries personal data: the API sets Cache-Control:
// no-store.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	tags := []string{"students"}
	self := func(o huma.Operation, act authz.Action, idem op.Idempotency) huma.Operation {
		o.Tags = tags
		return op.Koro(o, op.Meta{Action: string(act), Surface: op.SurfaceSelf, Idempotency: idem})
	}

	huma.Register(api, self(huma.Operation{
		OperationID: "getMyStudentProfile", Method: http.MethodGet, Path: "/me/student-profile",
		Summary: "Get my student profile",
		Description: "The full profile with its sections and privacy settings (only its owner sees it). A verified " +
			"student who never edited it gets the profile it starts with. Without an active student verification: " +
			"403 students.verification_required.",
	}, authz.StudentsProfileReadOwn, ""), h.getProfile)

	huma.Register(api, self(huma.Operation{
		OperationID: "updateMyStudentProfile", Method: http.MethodPatch, Path: "/me/student-profile",
		Summary: "Update my student profile",
		Description: "Partial update; an empty string clears an optional text. The university comes from the " +
			"student verification and cannot be changed (an unknown member such as university_id is 422). The " +
			"department must be an active department of that university (422 students.unknown_department).",
	}, authz.StudentsProfileUpdateOwn, ""), h.updateProfile)

	huma.Register(api, self(huma.Operation{
		OperationID: "putMySkills", Method: http.MethodPut, Path: "/me/student-profile/skills",
		Summary:     "Replace my skills",
		Description: "The list replaces the current skills (at most 30, active catalog skills only).",
	}, authz.StudentsProfileSectionManage, ""), h.putSkills)

	huma.Register(api, self(huma.Operation{
		OperationID: "createMyExperience", Method: http.MethodPost, Path: "/me/student-profile/experiences",
		Summary: "Add an experience", DefaultStatus: http.StatusCreated,
		Description: "At most 50. A club_role entry may name a sensitive club: it never reaches a company's " +
			"anonymous view and reaches level 3 only when show_clubs_on_grant is on.",
	}, authz.StudentsProfileSectionManage, op.IdempotencyOptional), h.createExperience)

	huma.Register(api, self(huma.Operation{
		OperationID: "updateMyExperience", Method: http.MethodPatch, Path: "/me/student-profile/experiences/{experience_id}",
		Summary:     "Update an experience",
		Description: "Partial update; an empty string clears an optional member. Another user's entry is 404.",
	}, authz.StudentsProfileSectionManage, ""), h.updateExperience)

	huma.Register(api, self(huma.Operation{
		OperationID: "deleteMyExperience", Method: http.MethodDelete, Path: "/me/student-profile/experiences/{experience_id}",
		Summary: "Delete an experience", DefaultStatus: http.StatusNoContent,
		Description: "Hard delete. Another user's entry is 404.",
	}, authz.StudentsProfileSectionManage, ""), h.deleteExperience)

	huma.Register(api, self(huma.Operation{
		OperationID: "putMyLanguages", Method: http.MethodPut, Path: "/me/student-profile/languages",
		Summary:     "Replace my languages",
		Description: "The list replaces the current languages (at most 20; ISO 639 codes, CEFR levels or native).",
	}, authz.StudentsProfileSectionManage, ""), h.putLanguages)

	huma.Register(api, self(huma.Operation{
		OperationID: "getMyPrivacy", Method: http.MethodGet, Path: "/me/privacy",
		Summary:     "Get my privacy settings",
		Description: "Everything is off until the student turns it on; the activity ranking choice starts at default.",
	}, authz.StudentsPrivacyReadOwn, ""), h.getPrivacy)

	huma.Register(api, self(huma.Operation{
		OperationID: "updateMyPrivacy", Method: http.MethodPatch, Path: "/me/privacy",
		Summary: "Update my privacy settings",
		Description: "discoverable (level 2) and show_contact_on_grant (the contact_info_share consent) mirror explicit " +
			"consents: turning one on needs the consent text shown (consent.discovery_profile / " +
			"consent.contact_info_share: notice_version_id and text_sha256), turning it off withdraws the consent; the " +
			"setting and the consent record change in one transaction. The e-mail goes only to the companies you share " +
			"it with when you accept their contact requests (share_contact; show_contact_on_grant is the default of " +
			"that box); turning show_contact_on_grant off stops every share at once. Users under 18 cannot turn either " +
			"on (422 students.minor_restricted). Every change is audited.",
	}, authz.StudentsPrivacyUpdateOwn, ""), h.updatePrivacy)

	registerViews(api, h)
	registerGrants(api, h)
	registerContactShare(api, h)
}

type handler struct{ svc Service }

// optionalMonth parses an optional month member ("" means clear).
func optionalMonth(loc string, s *string) (d *civil.Date, clear bool, err error) {
	if s == nil {
		return nil, false, nil
	}
	if *s == "" {
		return nil, true, nil
	}
	m, err := parseMonth(loc, *s)
	if err != nil {
		return nil, false, err
	}
	return &m, false, nil
}

// optionalID parses an optional UUID member ("" means clear).
func optionalID(loc string, s *string) (id *uuid.UUID, clear bool, err error) {
	if s == nil {
		return nil, false, nil
	}
	if *s == "" {
		return nil, true, nil
	}
	v, err := uuid.Parse(*s)
	if err != nil {
		return nil, false, fieldErr(loc, "must be a UUID")
	}
	return &v, false, nil
}

// ---- getMyStudentProfile / updateMyStudentProfile

type profileOutput struct {
	Body StudentProfileSelfDTO
}

func (h handler) getProfile(ctx context.Context, _ *struct{}) (*profileOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetMyProfile(ctx, a)
	if err != nil {
		return nil, err
	}
	return &profileOutput{Body: toSelfDTO(v)}, nil
}

type updateProfileInput struct {
	Body struct {
		DepartmentID       *string `json:"department_id,omitempty" doc:"A department of your university; empty string clears it"`
		DegreeLevel        *string `json:"degree_level,omitempty" enum:"associate,bachelor,master,doctorate"`
		ClassYear          *int16  `json:"class_year,omitempty" minimum:"1" maximum:"8"`
		ExpectedGraduation *string `json:"expected_graduation,omitempty" pattern:"^([0-9]{4}-(0[1-9]|1[0-2]))?$" doc:"YYYY-MM; empty string clears it"`
		Headline           *string `json:"headline,omitempty" maxLength:"160"`
		About              *string `json:"about,omitempty" maxLength:"2000"`
		City               *string `json:"city,omitempty" maxLength:"60"`
		AvatarFileID       *string `json:"avatar_file_id,omitempty" doc:"An avatar upload of yours (createUpload); empty string removes the avatar"`
	}
}

func (h handler) updateProfile(ctx context.Context, in *updateProfileInput) (*profileOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := ProfilePatch{ClassYear: b.ClassYear, Headline: b.Headline, About: b.About, City: b.City}
	if p.Department, p.ClearDepartment, err = optionalID("body.department_id", b.DepartmentID); err != nil {
		return nil, err
	}
	if p.Avatar, p.ClearAvatar, err = optionalID("body.avatar_file_id", b.AvatarFileID); err != nil {
		return nil, err
	}
	if p.ExpectedGraduation, p.ClearExpectedGraduation, err = optionalMonth("body.expected_graduation", b.ExpectedGraduation); err != nil {
		return nil, err
	}
	if b.DegreeLevel != nil {
		d := DegreeLevel(*b.DegreeLevel)
		p.DegreeLevel = &d
	}
	v, err := h.svc.UpdateMyProfile(ctx, a, p)
	if err != nil {
		return nil, err
	}
	return &profileOutput{Body: toSelfDTO(v)}, nil
}

// ---- putMySkills

type putSkillsInput struct {
	Body struct {
		Items []struct {
			SkillID string `json:"skill_id" format:"uuid"`
			Level   *int16 `json:"level,omitempty" minimum:"1" maximum:"5"`
		} `json:"items" maxItems:"30"`
	}
}

type skillsOutput struct {
	Body StudentSkillList
}

func (h handler) putSkills(ctx context.Context, in *putSkillsInput) (*skillsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]SkillInput, len(in.Body.Items))
	for i, it := range in.Body.Items {
		id, err := uuid.Parse(it.SkillID)
		if err != nil {
			return nil, fieldErr("body.items["+strconv.Itoa(i)+"].skill_id", "must be a UUID")
		}
		items[i] = SkillInput{SkillID: id, Level: it.Level}
	}
	rows, err := h.svc.PutMySkills(ctx, a, items)
	if err != nil {
		return nil, err
	}
	return &skillsOutput{Body: StudentSkillList{Items: skillDTOs(rows)}}, nil
}

// ---- experiences

type experienceOutput struct {
	Body StudentExperienceDTO
}

type createExperienceInput struct {
	idempotency.OptionalHeader
	Body struct {
		Kind             string `json:"kind" enum:"internship,job,volunteer,project,club_role"`
		Title            string `json:"title" minLength:"1" maxLength:"120"`
		OrganizationName string `json:"organization_name,omitempty" maxLength:"160"`
		StartMonth       string `json:"start_month" pattern:"^[0-9]{4}-(0[1-9]|1[0-2])$" doc:"YYYY-MM"`
		EndMonth         string `json:"end_month,omitempty" pattern:"^([0-9]{4}-(0[1-9]|1[0-2]))?$" doc:"YYYY-MM; absent: ongoing"`
		Description      string `json:"description,omitempty" maxLength:"2000"`
	}
}

func (h handler) createExperience(ctx context.Context, in *createExperienceInput) (*experienceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	start, err := parseMonth("body.start_month", b.StartMonth)
	if err != nil {
		return nil, err
	}
	end := (*civil.Date)(nil)
	if b.EndMonth != "" {
		m, err := parseMonth("body.end_month", b.EndMonth)
		if err != nil {
			return nil, err
		}
		end = &m
	}
	e, err := h.svc.CreateMyExperience(ctx, a, ExperienceInput{
		Kind: ExperienceKind(b.Kind), Title: b.Title, OrganizationName: b.OrganizationName, StartMonth: start,
		EndMonth: end, Description: b.Description,
	})
	if err != nil {
		return nil, err
	}
	return &experienceOutput{Body: toExperienceDTO(e)}, nil
}

// ExperiencePath is the {experience_id} path parameter (exported: huma
// binds parameters of embedded structs only when the type is exported).
type ExperiencePath struct {
	ExperienceID string `path:"experience_id" doc:"Experience ID (another user's or a malformed ID is 404)"`
}

func (p ExperiencePath) id() (uuid.UUID, error) {
	id, err := uuid.Parse(p.ExperienceID)
	if err != nil {
		return uuid.Nil(), ErrNotFound // IDOR rule: an unknown ID is simply not found
	}
	return id, nil
}

type updateExperienceInput struct {
	ExperiencePath
	Body struct {
		Kind             *string `json:"kind,omitempty" enum:"internship,job,volunteer,project,club_role"`
		Title            *string `json:"title,omitempty" minLength:"1" maxLength:"120"`
		OrganizationName *string `json:"organization_name,omitempty" maxLength:"160" doc:"Empty string clears it"`
		StartMonth       *string `json:"start_month,omitempty" pattern:"^[0-9]{4}-(0[1-9]|1[0-2])$" doc:"YYYY-MM"`
		EndMonth         *string `json:"end_month,omitempty" pattern:"^([0-9]{4}-(0[1-9]|1[0-2]))?$" doc:"YYYY-MM; empty string: ongoing"`
		Description      *string `json:"description,omitempty" maxLength:"2000" doc:"Empty string clears it"`
	}
}

func (h handler) updateExperience(ctx context.Context, in *updateExperienceInput) (*experienceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := ExperiencePatch{Title: b.Title, OrganizationName: b.OrganizationName, Description: b.Description}
	if b.Kind != nil {
		k := ExperienceKind(*b.Kind)
		p.Kind = &k
	}
	if b.StartMonth != nil {
		m, err := parseMonth("body.start_month", *b.StartMonth)
		if err != nil {
			return nil, err
		}
		p.StartMonth = &m
	}
	if p.EndMonth, p.ClearEndMonth, err = optionalMonth("body.end_month", b.EndMonth); err != nil {
		return nil, err
	}
	e, err := h.svc.UpdateMyExperience(ctx, a, id, p)
	if err != nil {
		return nil, err
	}
	return &experienceOutput{Body: toExperienceDTO(e)}, nil
}

func (h handler) deleteExperience(ctx context.Context, in *ExperiencePath) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := in.id()
	if err != nil {
		return nil, err
	}
	if err := h.svc.DeleteMyExperience(ctx, a, id); err != nil {
		return nil, err
	}
	return nil, nil
}

// ---- putMyLanguages

type putLanguagesInput struct {
	Body struct {
		Items []StudentLanguageDTO `json:"items" maxItems:"20"`
	}
}

type languagesOutput struct {
	Body StudentLanguageList
}

func (h handler) putLanguages(ctx context.Context, in *putLanguagesInput) (*languagesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]LanguageInput, len(in.Body.Items))
	for i, it := range in.Body.Items {
		items[i] = LanguageInput{LanguageCode: it.LanguageCode, Level: LanguageLevel(it.Level)}
	}
	rows, err := h.svc.PutMyLanguages(ctx, a, items)
	if err != nil {
		return nil, err
	}
	return &languagesOutput{Body: StudentLanguageList{Items: languageDTOs(rows)}}, nil
}

// ---- privacy

type privacyOutput struct {
	Body StudentPrivacyDTO
}

func (h handler) getPrivacy(ctx context.Context, _ *struct{}) (*privacyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.GetMyPrivacy(ctx, a)
	if err != nil {
		return nil, err
	}
	return &privacyOutput{Body: toPrivacyDTO(p)}, nil
}

// studentConsentProof is the consent text a student saw (04 §2.4).
type studentConsentProof struct {
	NoticeVersionID string `json:"notice_version_id" format:"uuid" doc:"The current consent text of the purpose"`
	TextSHA256      string `json:"text_sha256" pattern:"^[0-9a-f]{64}$" doc:"body_sha256 of that text"`
}

type updatePrivacyInput struct {
	Client string `header:"X-Koro-Client" enum:"web,mobile" doc:"Client kind; recorded as the consent channel (default web)"`
	Body   struct {
		Discoverable          *bool   `json:"discoverable,omitempty"`
		ShowContactOnGrant    *bool   `json:"show_contact_on_grant,omitempty"`
		ShowClubsOnGrant      *bool   `json:"show_clubs_on_grant,omitempty"`
		ActivityRankingChoice *string `json:"activity_ranking_choice,omitempty" enum:"default,enabled,disabled"`
		Consent               *struct {
			DiscoveryProfile *studentConsentProof `json:"discovery_profile,omitempty" doc:"Required to turn discoverable on"`
			ContactInfoShare *studentConsentProof `json:"contact_info_share,omitempty" doc:"Required to turn show_contact_on_grant on"`
		} `json:"consent,omitempty" doc:"The consent text shown for every consent turned on"`
	}
}

func proofOf(loc string, b *studentConsentProof) (*ConsentProof, error) {
	if b == nil {
		return nil, nil
	}
	id, err := uuid.Parse(b.NoticeVersionID)
	if err != nil {
		return nil, fieldErr(loc+".notice_version_id", "must be a UUID")
	}
	sum, err := hex.DecodeString(b.TextSHA256)
	if err != nil || len(sum) != 32 {
		return nil, fieldErr(loc+".text_sha256", "must be 64 hex characters")
	}
	return &ConsentProof{NoticeVersionID: id, TextSHA256: sum}, nil
}

func (h handler) updatePrivacy(ctx context.Context, in *updatePrivacyInput) (*privacyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := PrivacyPatch{
		Discoverable: b.Discoverable, ShowContactOnGrant: b.ShowContactOnGrant, ShowClubsOnGrant: b.ShowClubsOnGrant,
		Channel: consent.ChannelWeb,
	}
	if in.Client == "mobile" {
		p.Channel = consent.ChannelMobile
	}
	if b.ActivityRankingChoice != nil {
		c := RankingChoice(*b.ActivityRankingChoice)
		p.ActivityRankingChoice = &c
	}
	if c := b.Consent; c != nil {
		if p.DiscoveryConsent, err = proofOf("body.consent.discovery_profile", c.DiscoveryProfile); err != nil {
			return nil, err
		}
		if p.ContactConsent, err = proofOf("body.consent.contact_info_share", c.ContactInfoShare); err != nil {
			return nil, err
		}
	}
	out, err := h.svc.UpdateMyPrivacy(ctx, a, p)
	if err != nil {
		return nil, err
	}
	return &privacyOutput{Body: toPrivacyDTO(out)}, nil
}
