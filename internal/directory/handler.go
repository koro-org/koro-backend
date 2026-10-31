package directory

import (
	"context"
	"net/http"
	"strings"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register adds the directory operations (04 §2.3: BE-020's catalog, BE-022's
// club claims) to api. svc may be nil (`koro openapi`): handlers only touch
// it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	op.Register(api, "directory", huma.Operation{
		OperationID: "listUniversities", Method: http.MethodGet, Path: "/universities",
		Summary: "List universities",
		Description: "Active universities. q matches name, short name and city (Turkish full text) or part of the name. " +
			"org_id finds the university of a university organization (getOrg has no university_id).",
	}, op.Meta{Action: string(authz.DirectoryUniversityList), Surface: op.SurfacePublic}, h.listUniversities)

	op.Register(api, "directory", huma.Operation{
		OperationID: "getUniversity", Method: http.MethodGet, Path: "/universities/{university_id}",
		Summary: "Get a university with its faculties",
	}, op.Meta{Action: string(authz.DirectoryUniversityRead), Surface: op.SurfacePublic}, h.getUniversity)

	op.Register(api, "directory", huma.Operation{
		OperationID: "listDepartments", Method: http.MethodGet, Path: "/universities/{university_id}/departments",
		Summary: "List the departments of a university",
	}, op.Meta{Action: string(authz.DirectoryDepartmentList), Surface: op.SurfacePublic}, h.listDepartments)

	op.Register(api, "directory", huma.Operation{
		OperationID: "getDepartment", Method: http.MethodGet, Path: "/departments/{department_id}",
		Summary: "Get a department with its university",
		Description: "Names a stored department_id (a student profile, a segment; BE-166). A closed department is " +
			"returned with is_active false; a department of an inactive university is 404 except for platform admins.",
	}, op.Meta{Action: string(authz.DirectoryDepartmentList), Surface: op.SurfacePublic}, h.getDepartment)

	op.Register(api, "directory", huma.Operation{
		OperationID: "listSkills", Method: http.MethodGet, Path: "/skills",
		Summary: "List the skill taxonomy", Description: "name follows Accept-Language (tr or en).",
	}, op.Meta{Action: string(authz.DirectorySkillList), Surface: op.SurfacePublic}, h.listSkills)

	op.Register(api, "directory", huma.Operation{
		OperationID: "listClubs", Method: http.MethodGet, Path: "/clubs",
		Summary: "List clubs",
		Description: "q matches the club name with Turkish rules (İSTANBUL, Istanbul and istanbul are the same word). " +
			"member_count is null below 10 members and for sensitive clubs.",
	}, op.Meta{Action: string(authz.DirectoryClubList), Surface: op.SurfacePublic}, h.listClubs)

	op.Register(api, "directory", huma.Operation{
		OperationID: "getClub", Method: http.MethodGet, Path: "/clubs/{club_id}",
		Summary:     "Get a club",
		Description: "club_id is the club's org_id. The ETag is the If-Match value of updateClub.",
	}, op.Meta{Action: string(authz.DirectoryClubRead), Surface: op.SurfacePublic}, h.getClub)

	op.Register(api, "directory", huma.Operation{
		OperationID: "updateClub", Method: http.MethodPatch, Path: "/clubs/{club_id}",
		Summary: "Update a club's catalog data",
		Description: "If-Match with the ETag of getClub is required: missing → 428, stale → 412. A religious, political " +
			"or identity category needs the sensitive flag first (422 directory.sensitivity_required).",
	}, op.Meta{Action: string(authz.DirectoryClubUpdate), Surface: op.SurfaceResource}, h.updateClub)

	op.Register(api, "directory", huma.Operation{
		OperationID: "requestClubSensitivity", Method: http.MethodPost, Path: "/clubs/{club_id}/sensitivity-requests",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Ask for the sensitive club flag",
		Description:   "Audited; the platform admins get an in-app notice. Only a platform admin sets the flag.",
	}, op.Meta{Action: string(authz.DirectoryClubRequestSensitivity), Surface: op.SurfaceResource,
		Idempotency: op.IdempotencyOptional}, h.requestSensitivity)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminCreateClub", Method: http.MethodPost, Path: "/admin/clubs", DefaultStatus: http.StatusCreated,
		Summary: "Add a club to the catalog", Description: "Opens the club's org and its catalog row together.",
	}, op.Meta{Action: string(authz.DirectoryClubCreate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminCreateClub)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminSetClubSensitivity", Method: http.MethodPost, Path: "/admin/clubs/{club_id}/sensitivity",
		Summary: "Set or clear the sensitive club flag",
		Description: "With re-authentication. A change of the flag emits club.sensitivity_changed in the same " +
			"transaction (the club's audience keys are removed). Religious, political and identity clubs stay sensitive.",
	}, op.Meta{Action: string(authz.DirectoryClubSetSensitivity), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.adminSetSensitivity)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminCreateUniversity", Method: http.MethodPost, Path: "/admin/universities",
		DefaultStatus: http.StatusCreated,
		Summary:       "Add a university", Description: "Bulk import is the `koro seed reference` command.",
	}, op.Meta{Action: string(authz.DirectoryUniversityManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminCreateUniversity)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminUpdateUniversity", Method: http.MethodPatch, Path: "/admin/universities/{university_id}",
		Summary: "Update a university", Description: "The org profile (name, logo) is changed with updateOrg.",
	}, op.Meta{Action: string(authz.DirectoryUniversityManage), Surface: op.SurfaceAdmin}, h.adminUpdateUniversity)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminPutUniversityEmailDomain", Method: http.MethodPut,
		Path:        "/admin/universities/{university_id}/email-domains/{domain}",
		Summary:     "Add or change a university's e-mail domain",
		Description: "A .edu.tr domain; a domain of another university is 409 directory.domain_taken.",
	}, op.Meta{Action: string(authz.DirectoryUniversityManage), Surface: op.SurfaceAdmin}, h.adminPutDomain)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminCreateDepartment", Method: http.MethodPost, Path: "/admin/universities/{university_id}/departments",
		DefaultStatus: http.StatusCreated,
		Summary:       "Add a department",
	}, op.Meta{Action: string(authz.DirectoryUniversityManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminCreateDepartment)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminListUniversities", Method: http.MethodGet, Path: "/admin/universities",
		Summary: "List universities, inactive ones included",
		Description: "The catalog's managers' list (BE-128): state active, inactive or all (default all); q, city, " +
			"type and sort as in listUniversities.",
	}, op.Meta{Action: string(authz.DirectoryUniversityManage), Surface: op.SurfaceAdmin}, h.adminListUniversities)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminListUniversityEmailDomains", Method: http.MethodGet,
		Path:        "/admin/universities/{university_id}/email-domains",
		Summary:     "List a university's e-mail domains",
		Description: "Every domain, active or not, by domain (BE-128). An unknown university is 404.",
	}, op.Meta{Action: string(authz.DirectoryUniversityManage), Surface: op.SurfaceAdmin}, h.adminListDomains)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminCreateFaculty", Method: http.MethodPost, Path: "/admin/universities/{university_id}/faculties",
		DefaultStatus: http.StatusCreated,
		Summary:       "Add a faculty",
		Description: "Active at once; adminCreateDepartment can name it as faculty_id (BE-128). Another faculty of the " +
			"university with that name is 409 directory.faculty_exists. Bulk import is the `koro seed reference` command.",
	}, op.Meta{Action: string(authz.DirectoryUniversityManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminCreateFaculty)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminListSkills", Method: http.MethodGet, Path: "/admin/skills",
		Summary: "List the skill taxonomy, inactive skills included",
		Description: "The taxonomy's managers' list (BE-128): both names (name_tr, name_en) and is_active; state " +
			"active, inactive or all (default all); q and category as in listSkills. Newest first.",
	}, op.Meta{Action: string(authz.DirectorySkillManage), Surface: op.SurfaceAdmin}, h.adminListSkills)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminCreateSkill", Method: http.MethodPost, Path: "/admin/skills", DefaultStatus: http.StatusCreated,
		Summary: "Add a skill",
	}, op.Meta{Action: string(authz.DirectorySkillManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminCreateSkill)

	op.Register(api, "directory", huma.Operation{
		OperationID: "adminUpdateSkill", Method: http.MethodPatch, Path: "/admin/skills/{skill_id}",
		Summary: "Update or deactivate a skill",
	}, op.Meta{Action: string(authz.DirectorySkillManage), Surface: op.SurfaceAdmin}, h.adminUpdateSkill)

	registerClaims(api, h)
}

type handler struct{ svc Service }

// localeOf maps Accept-Language to tr or en (04 §1.12; tr otherwise).
func localeOf(acceptLanguage string) string {
	for tag := range strings.SplitSeq(acceptLanguage, ",") {
		lang, _, _ := strings.Cut(strings.TrimSpace(tag), ";")
		lang = strings.ToLower(strings.TrimSpace(lang))
		switch {
		case lang == "tr" || strings.HasPrefix(lang, "tr-"):
			return "tr"
		case lang == "en" || strings.HasPrefix(lang, "en-"):
			return "en"
		}
	}
	return "tr"
}

// sortParam maps the sort query parameter (04 §1.7: "name") to a store
// order; anything else is newest first.
func sortParam(s string) string {
	if s == "name" {
		return SortName
	}
	return SortNewest
}

// cursorOf decodes a cursor of sort with the request's filter fingerprint.
func cursorOf(raw, sort, fp string) (*After, error) {
	n := 1
	if sort == SortName {
		n = 2
	}
	c, err := page.Decode(raw, sort, fp, n)
	if err != nil || c == nil {
		return nil, err
	}
	id, err := c.UUID(n - 1)
	if err != nil {
		return nil, err
	}
	a := &After{ID: id}
	if sort == SortName {
		a.Name = c.Vals[0]
	}
	return a, nil
}

func nextCursor(next *After, sort, fp string) string {
	switch {
	case next == nil:
		return ""
	case sort == SortName:
		return page.Next(SortName, fp, next.Name, page.UUIDVal(next.ID))
	}
	return page.Next(SortNewest, fp, page.UUIDVal(next.ID))
}

func proofOf(r *consent.ReauthDTO) identity.ReauthProof {
	if r == nil {
		return identity.ReauthProof{}
	}
	return identity.ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
}

// ---- universities and departments

type listUniversitiesInput struct {
	page.Params
	Q    string `query:"q" maxLength:"100" doc:"Free text, 2-100 characters"`
	City string `query:"city" maxLength:"60"`
	Type string `query:"type" enum:"state,foundation,other"`
	Sort string `query:"sort" enum:"name" doc:"name (A-Z, Turkish order); default newest first"`
	// OrgID resolves a university panel's own university (getOrg carries
	// no university_id; BE-127).
	OrgID string `query:"org_id" format:"uuid" doc:"Only the university of this organization (its org_id): at most one item"`
}

type listUniversitiesOutput struct{ Body UniversityList }

func (h handler) listUniversities(ctx context.Context, in *listUniversitiesInput) (*listUniversitiesOutput, error) {
	sort := sortParam(in.Sort)
	fp := page.Fingerprint("listUniversities", in.Q, in.City, in.Type, in.OrgID)
	after, err := cursorOf(in.Cursor, sort, fp)
	if err != nil {
		return nil, err
	}
	org, err := httpx.OptID("query.org_id", in.OrgID)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListUniversities(ctx, authz.ActorOrAnonymous(ctx), UniversityQuery{
		Q: in.Q, City: in.City, Type: UniversityType(in.Type), OrgID: org, Sort: sort, Limit: in.Limit, After: after,
	})
	if err != nil {
		return nil, err
	}
	out := &listUniversitiesOutput{Body: UniversityList{Items: make([]UniversityCardDTO, len(p.Items))}}
	for i, u := range p.Items {
		out.Body.Items[i] = toUniversityCard(u)
	}
	out.Body.NextCursor = nextCursor(p.Next, p.Sort, fp)
	return out, nil
}

// UniversityPath is the university_id path parameter (exported: huma reads
// the fields of an embedded struct only when its type is exported).
type UniversityPath struct {
	UniversityID string `path:"university_id" doc:"University ID"`
}

type universityOutput struct{ Body UniversityDTO }

func (h handler) getUniversity(ctx context.Context, in *UniversityPath) (*universityOutput, error) {
	v, err := h.svc.GetUniversityView(ctx, authz.ActorOrAnonymous(ctx), httpx.ParseID(in.UniversityID))
	if err != nil {
		return nil, err
	}
	return &universityOutput{Body: toUniversityDTO(v.University, v.Faculties)}, nil
}

type listDepartmentsInput struct {
	UniversityPath
	page.Params
	Q           string `query:"q" maxLength:"100" doc:"Free text, 2-100 characters"`
	FacultyID   string `query:"faculty_id" format:"uuid"`
	DegreeLevel string `query:"degree_level" enum:"associate,bachelor,master,doctorate"`
}

type listDepartmentsOutput struct{ Body DepartmentList }

func (h handler) listDepartments(ctx context.Context, in *listDepartmentsInput) (*listDepartmentsOutput, error) {
	faculty, err := httpx.OptID("query.faculty_id", in.FacultyID)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listDepartments", in.UniversityID, in.Q, in.FacultyID, in.DegreeLevel)
	after, err := cursorOf(in.Cursor, SortNewest, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListDepartments(ctx, authz.ActorOrAnonymous(ctx), httpx.ParseID(in.UniversityID), DepartmentQuery{
		Q: in.Q, FacultyID: faculty, DegreeLevel: DegreeLevel(in.DegreeLevel), Limit: in.Limit, After: after,
	})
	if err != nil {
		return nil, err
	}
	out := &listDepartmentsOutput{Body: DepartmentList{Items: make([]DepartmentDTO, len(p.Items))}}
	for i, d := range p.Items {
		out.Body.Items[i] = toDepartmentDTO(d, p.University)
	}
	out.Body.NextCursor = nextCursor(p.Next, SortNewest, fp)
	return out, nil
}

// DepartmentPath is the department_id path parameter (exported: huma reads
// the fields of an embedded struct only when its type is exported).
type DepartmentPath struct {
	DepartmentID string `path:"department_id" doc:"Department ID"`
}

func (h handler) getDepartment(ctx context.Context, in *DepartmentPath) (*departmentOutput, error) {
	v, err := h.svc.GetDepartmentView(ctx, authz.ActorOrAnonymous(ctx), httpx.ParseID(in.DepartmentID))
	if err != nil {
		return nil, err
	}
	return &departmentOutput{Body: toDepartmentDTO(v.Department, v.University)}, nil
}

// ---- skills

type listSkillsInput struct {
	page.Params
	Q              string `query:"q" maxLength:"100" doc:"Free text, 2-100 characters"`
	Category       string `query:"category" enum:"technical,tool,soft,domain,language_tool,other"`
	AcceptLanguage string `header:"Accept-Language" maxLength:"1000" doc:"tr or en; tr otherwise"`
}

type listSkillsOutput struct {
	ContentLanguage string `header:"Content-Language"`
	Body            SkillList
}

func (h handler) listSkills(ctx context.Context, in *listSkillsInput) (*listSkillsOutput, error) {
	fp := page.Fingerprint("listSkills", in.Q, in.Category)
	after, err := cursorOf(in.Cursor, SortNewest, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListSkills(ctx, authz.ActorOrAnonymous(ctx), SkillQuery{
		Q: in.Q, Category: SkillCategory(in.Category), Limit: in.Limit, After: after,
	})
	if err != nil {
		return nil, err
	}
	locale := localeOf(in.AcceptLanguage)
	out := &listSkillsOutput{ContentLanguage: locale, Body: SkillList{Items: make([]SkillDTO, len(p.Items))}}
	for i, k := range p.Items {
		out.Body.Items[i] = toSkillDTO(k, locale)
	}
	out.Body.NextCursor = nextCursor(p.Next, SortNewest, fp)
	return out, nil
}

// ---- clubs

type listClubsInput struct {
	page.Params
	Q            string `query:"q" maxLength:"100" doc:"Free text, 2-100 characters"`
	UniversityID string `query:"university_id" format:"uuid"`
	Category     string `query:"category" enum:"academic,technology,career,entrepreneurship,arts,culture,sports,social_responsibility,hobby,religious,political,identity,other"`
	Claimed      string `query:"claimed" enum:"true,false"`
	Sort         string `query:"sort" enum:"name" doc:"name (A-Z, Turkish order); default newest first"`
}

type listClubsOutput struct{ Body ClubCardList }

func (h handler) listClubs(ctx context.Context, in *listClubsInput) (*listClubsOutput, error) {
	uni, err := httpx.OptID("query.university_id", in.UniversityID)
	if err != nil {
		return nil, err
	}
	var claimed *bool
	if in.Claimed != "" {
		claimed = ptr(in.Claimed == "true")
	}
	sort := sortParam(in.Sort)
	fp := page.Fingerprint("listClubs", in.Q, in.UniversityID, in.Category, in.Claimed)
	after, err := cursorOf(in.Cursor, sort, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListClubs(ctx, authz.ActorOrAnonymous(ctx), ClubQuery{
		Q: in.Q, UniversityID: uni, Category: Category(in.Category), Claimed: claimed, Sort: sort, Limit: in.Limit, After: after,
	})
	if err != nil {
		return nil, err
	}
	out := &listClubsOutput{Body: ClubCardList{Items: make([]ClubCardDTO, len(p.Items))}}
	for i, c := range p.Items {
		out.Body.Items[i] = toClubCard(c)
	}
	out.Body.NextCursor = nextCursor(p.Next, p.Sort, fp)
	return out, nil
}

// ClubPath is the club_id path parameter (the club's org_id).
type ClubPath struct {
	ClubID string `path:"club_id" doc:"The club's org ID"`
}

type clubOutput struct {
	ETag string `header:"ETag"`
	Body ClubDTO
}

func clubOut(v ClubView) *clubOutput { return &clubOutput{ETag: v.ETag, Body: toClubDTO(v)} }

func (h handler) getClub(ctx context.Context, in *ClubPath) (*clubOutput, error) {
	v, err := h.svc.GetClubView(ctx, authz.ActorOrAnonymous(ctx), httpx.ParseID(in.ClubID))
	if err != nil {
		return nil, err
	}
	return clubOut(v), nil
}

type updateClubInput struct {
	ClubPath
	IfMatch string `header:"If-Match" doc:"ETag of getClub (required: missing → 428, stale → 412)"`
	Body    struct {
		Category    *string         `json:"category,omitempty" enum:"academic,technology,career,entrepreneurship,arts,culture,sports,social_responsibility,hobby,religious,political,identity,other"`
		JoinPolicy  *string         `json:"join_policy,omitempty" enum:"open,approval"`
		SocialLinks *socialLinksMap `json:"social_links,omitempty" doc:"Replaces every link; network name → https URL; {} clears them"`
		FoundedYear *int16          `json:"founded_year,omitempty" minimum:"0" maximum:"2100" doc:"0 clears it"`
	}
}

func (h handler) updateClub(ctx context.Context, in *updateClubInput) (*clubOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	var p ClubPatch
	if b.Category != nil {
		p.Category = ptr(Category(*b.Category))
	}
	if b.JoinPolicy != nil {
		p.JoinPolicy = ptr(JoinPolicy(*b.JoinPolicy))
	}
	if b.SocialLinks != nil {
		p.SetSocialLinks, p.SocialLinks = true, map[string]string(*b.SocialLinks)
	}
	if b.FoundedYear != nil {
		if *b.FoundedYear == 0 {
			p.ClearFoundedYear = true
		} else {
			p.FoundedYear = b.FoundedYear
		}
	}
	v, err := h.svc.UpdateClub(ctx, a, httpx.ParseID(in.ClubID), in.IfMatch, p)
	if err != nil {
		return nil, err
	}
	return clubOut(v), nil
}

type requestSensitivityInput struct {
	ClubPath
	Body struct {
		SensitiveReason string `json:"sensitive_reason" enum:"religion,political_opinion,philosophical_belief,lgbtiq,ethnic_origin,health,union,other"`
		Note            string `json:"note,omitempty" maxLength:"500" doc:"Shown to the platform admins in the audit log"`
	}
}

type acceptedOutput struct{}

func (h handler) requestSensitivity(ctx context.Context, in *requestSensitivityInput) (*acceptedOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.RequestClubSensitivity(ctx, a, httpx.ParseID(in.ClubID), SensitiveReason(in.Body.SensitiveReason), in.Body.Note); err != nil {
		return nil, err
	}
	return &acceptedOutput{}, nil
}

type adminCreateClubInput struct {
	Body struct {
		UniversityID    string `json:"university_id" format:"uuid"`
		Name            string `json:"name" minLength:"2" maxLength:"200"`
		Slug            string `json:"slug,omitempty" maxLength:"80" doc:"Derived from the name when omitted"`
		Category        string `json:"category" enum:"academic,technology,career,entrepreneurship,arts,culture,sports,social_responsibility,hobby,religious,political,identity,other"`
		JoinPolicy      string `json:"join_policy" enum:"open,approval"`
		IsSensitive     bool   `json:"is_sensitive,omitempty"`
		SensitiveReason string `json:"sensitive_reason,omitempty" enum:"religion,political_opinion,philosophical_belief,lgbtiq,ethnic_origin,health,union,other"`
	}
}

func (h handler) adminCreateClub(ctx context.Context, in *adminCreateClubInput) (*clubOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	uni, err := uuid.Parse(b.UniversityID)
	if err != nil {
		return nil, problem.Field("body.university_id", "must be a UUID")
	}
	v, err := h.svc.AdminCreateClub(ctx, a, ClubInput{
		UniversityID: uni, Name: b.Name, Slug: b.Slug, Category: Category(b.Category), JoinPolicy: JoinPolicy(b.JoinPolicy),
		IsSensitive: b.IsSensitive, SensitiveReason: SensitiveReason(b.SensitiveReason),
	})
	if err != nil {
		return nil, err
	}
	return clubOut(v), nil
}

type adminSensitivityInput struct {
	ClubPath
	Body struct {
		IsSensitive     bool               `json:"is_sensitive"`
		SensitiveReason string             `json:"sensitive_reason,omitempty" enum:"religion,political_opinion,philosophical_belief,lgbtiq,ethnic_origin,health,union,other"`
		Reauth          *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) adminSetSensitivity(ctx context.Context, in *adminSensitivityInput) (*clubOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	v, err := h.svc.AdminSetClubSensitivity(ctx, a, httpx.ParseID(in.ClubID),
		SensitivityInput{IsSensitive: b.IsSensitive, SensitiveReason: SensitiveReason(b.SensitiveReason)}, proofOf(b.Reauth))
	if err != nil {
		return nil, err
	}
	return clubOut(v), nil
}

// ---- admin universities, domains, departments

type adminCreateUniversityInput struct {
	Body struct {
		YokCode    string `json:"yok_code" minLength:"1" maxLength:"40"`
		Name       string `json:"name" minLength:"2" maxLength:"200"`
		ShortName  string `json:"short_name,omitempty" maxLength:"60"`
		City       string `json:"city" minLength:"2" maxLength:"60"`
		Type       string `json:"type" enum:"state,foundation,other"`
		WebsiteURL string `json:"website_url,omitempty" maxLength:"2048"`
	}
}

func (h handler) adminCreateUniversity(ctx context.Context, in *adminCreateUniversityInput) (*universityOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	u, err := h.svc.AdminCreateUniversity(ctx, a, UniversityInput{
		YokCode: b.YokCode, Name: b.Name, ShortName: b.ShortName, City: b.City, Type: UniversityType(b.Type), WebsiteURL: b.WebsiteURL,
	})
	if err != nil {
		return nil, err
	}
	return &universityOutput{Body: toUniversityDTO(u, nil)}, nil
}

type adminUpdateUniversityInput struct {
	UniversityPath
	Body struct {
		Name       *string `json:"name,omitempty" minLength:"2" maxLength:"200"`
		ShortName  *string `json:"short_name,omitempty" maxLength:"60" doc:"Empty string clears it"`
		City       *string `json:"city,omitempty" minLength:"2" maxLength:"60"`
		Type       *string `json:"type,omitempty" enum:"state,foundation,other"`
		WebsiteURL *string `json:"website_url,omitempty" maxLength:"2048" doc:"Empty string clears it"`
		IsActive   *bool   `json:"is_active,omitempty"`
	}
}

func (h handler) adminUpdateUniversity(ctx context.Context, in *adminUpdateUniversityInput) (*universityOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := UniversityPatch{Name: b.Name, ShortName: b.ShortName, City: b.City, WebsiteURL: b.WebsiteURL, IsActive: b.IsActive}
	if b.Type != nil {
		p.Type = ptr(UniversityType(*b.Type))
	}
	u, err := h.svc.AdminUpdateUniversity(ctx, a, httpx.ParseID(in.UniversityID), p)
	if err != nil {
		return nil, err
	}
	return &universityOutput{Body: toUniversityDTO(u, nil)}, nil
}

type adminPutDomainInput struct {
	UniversityPath
	Domain string `path:"domain" maxLength:"253" doc:"A .edu.tr domain, e.g. itu.edu.tr"`
	Body   struct {
		DomainKind string `json:"domain_kind" enum:"student,staff,mixed"`
		IsActive   bool   `json:"is_active"`
	}
}

type domainOutput struct{ Body EmailDomainDTO }

func (h handler) adminPutDomain(ctx context.Context, in *adminPutDomainInput) (*domainOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, _, err := h.svc.AdminPutUniversityEmailDomain(ctx, a, httpx.ParseID(in.UniversityID), in.Domain,
		DomainInput{DomainKind: DomainKind(in.Body.DomainKind), IsActive: in.Body.IsActive})
	if err != nil {
		return nil, err
	}
	return &domainOutput{Body: toDomainDTO(d)}, nil
}

type adminCreateDepartmentInput struct {
	UniversityPath
	Body struct {
		FacultyID      string `json:"faculty_id,omitempty" format:"uuid" doc:"A faculty of this university"`
		Name           string `json:"name" minLength:"2" maxLength:"200"`
		DegreeLevel    string `json:"degree_level" enum:"associate,bachelor,master,doctorate"`
		DurationYears  *int16 `json:"duration_years,omitempty" minimum:"1" maximum:"7"`
		YokProgramCode string `json:"yok_program_code,omitempty" maxLength:"40"`
	}
}

type departmentOutput struct{ Body DepartmentDTO }

func (h handler) adminCreateDepartment(ctx context.Context, in *adminCreateDepartmentInput) (*departmentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	faculty, err := httpx.OptID("body.faculty_id", b.FacultyID)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminCreateDepartment(ctx, a, httpx.ParseID(in.UniversityID), DepartmentInput{
		FacultyID: faculty, Name: b.Name, DegreeLevel: DegreeLevel(b.DegreeLevel), DurationYears: b.DurationYears,
		YokProgramCode: b.YokProgramCode,
	})
	if err != nil {
		return nil, err
	}
	return &departmentOutput{Body: toDepartmentDTO(v.Department, v.University)}, nil
}

// adminState maps the state query parameter of the admin lists (default
// all).
func adminState(s string) ActiveState {
	switch s {
	case "active":
		return StateActive
	case "inactive":
		return StateInactive
	}
	return StateAll
}

type adminListUniversitiesInput struct {
	page.Params
	Q     string `query:"q" maxLength:"100" doc:"Free text, 2-100 characters"`
	City  string `query:"city" maxLength:"60"`
	Type  string `query:"type" enum:"state,foundation,other"`
	Sort  string `query:"sort" enum:"name" doc:"name (A-Z, Turkish order); default newest first"`
	State string `query:"state" enum:"active,inactive,all" doc:"is_active filter; default all"`
}

type adminUniversityListOutput struct{ Body AdminUniversityList }

func (h handler) adminListUniversities(ctx context.Context, in *adminListUniversitiesInput) (*adminUniversityListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	sort := sortParam(in.Sort)
	fp := page.Fingerprint("adminListUniversities", in.Q, in.City, in.Type, in.State)
	after, err := cursorOf(in.Cursor, sort, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListUniversities(ctx, a, UniversityQuery{
		Q: in.Q, City: in.City, Type: UniversityType(in.Type), Sort: sort, State: adminState(in.State), Limit: in.Limit, After: after,
	})
	if err != nil {
		return nil, err
	}
	out := &adminUniversityListOutput{Body: AdminUniversityList{Items: make([]AdminUniversityCardDTO, len(p.Items))}}
	for i, u := range p.Items {
		out.Body.Items[i] = AdminUniversityCardDTO{UniversityCardDTO: toUniversityCard(u), YokCode: u.YokCode, IsActive: u.IsActive}
	}
	out.Body.NextCursor = nextCursor(p.Next, p.Sort, fp)
	return out, nil
}

type domainListOutput struct{ Body EmailDomainList }

func (h handler) adminListDomains(ctx context.Context, in *UniversityPath) (*domainListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ds, err := h.svc.AdminListUniversityEmailDomains(ctx, a, httpx.ParseID(in.UniversityID))
	if err != nil {
		return nil, err
	}
	out := &domainListOutput{Body: EmailDomainList{Items: make([]EmailDomainDTO, len(ds))}}
	for i, d := range ds {
		out.Body.Items[i] = toDomainDTO(d)
	}
	return out, nil
}

type adminCreateFacultyInput struct {
	UniversityPath
	Body struct {
		Name    string `json:"name" minLength:"2" maxLength:"200"`
		YokCode string `json:"yok_code,omitempty" maxLength:"40"`
	}
}

type facultyOutput struct{ Body FacultyDTO }

func (h handler) adminCreateFaculty(ctx context.Context, in *adminCreateFacultyInput) (*facultyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	f, err := h.svc.AdminCreateFaculty(ctx, a, httpx.ParseID(in.UniversityID), FacultyInput{Name: in.Body.Name, YokCode: in.Body.YokCode})
	if err != nil {
		return nil, err
	}
	return &facultyOutput{Body: toFacultyDTO(f)}, nil
}

// ---- admin skills

type adminListSkillsInput struct {
	page.Params
	Q        string `query:"q" maxLength:"100" doc:"Free text, 2-100 characters"`
	Category string `query:"category" enum:"technical,tool,soft,domain,language_tool,other"`
	State    string `query:"state" enum:"active,inactive,all" doc:"is_active filter; default all"`
}

type adminSkillListOutput struct{ Body AdminSkillList }

func (h handler) adminListSkills(ctx context.Context, in *adminListSkillsInput) (*adminSkillListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListSkills", in.Q, in.Category, in.State)
	after, err := cursorOf(in.Cursor, SortNewest, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListSkills(ctx, a, SkillQuery{
		Q: in.Q, Category: SkillCategory(in.Category), State: adminState(in.State), Limit: in.Limit, After: after,
	})
	if err != nil {
		return nil, err
	}
	out := &adminSkillListOutput{Body: AdminSkillList{Items: make([]AdminSkillDTO, len(p.Items))}}
	for i, k := range p.Items {
		out.Body.Items[i] = toAdminSkillDTO(k)
	}
	out.Body.NextCursor = nextCursor(p.Next, SortNewest, fp)
	return out, nil
}

type adminCreateSkillInput struct {
	Body struct {
		Slug     string `json:"slug" maxLength:"80" pattern:"^[a-z0-9]+(-[a-z0-9]+)*$"`
		NameTR   string `json:"name_tr" minLength:"1" maxLength:"200"`
		NameEN   string `json:"name_en,omitempty" maxLength:"200"`
		Category string `json:"category" enum:"technical,tool,soft,domain,language_tool,other"`
	}
}

type adminSkillOutput struct{ Body AdminSkillDTO }

func (h handler) adminCreateSkill(ctx context.Context, in *adminCreateSkillInput) (*adminSkillOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	k, err := h.svc.AdminCreateSkill(ctx, a, SkillInput{Slug: b.Slug, NameTR: b.NameTR, NameEN: b.NameEN, Category: SkillCategory(b.Category)})
	if err != nil {
		return nil, err
	}
	return &adminSkillOutput{Body: toAdminSkillDTO(k)}, nil
}

type adminUpdateSkillInput struct {
	SkillID string `path:"skill_id" doc:"Skill ID"`
	Body    struct {
		NameTR   *string `json:"name_tr,omitempty" minLength:"1" maxLength:"200"`
		NameEN   *string `json:"name_en,omitempty" maxLength:"200" doc:"Empty string clears it"`
		Category *string `json:"category,omitempty" enum:"technical,tool,soft,domain,language_tool,other"`
		IsActive *bool   `json:"is_active,omitempty"`
	}
}

func (h handler) adminUpdateSkill(ctx context.Context, in *adminUpdateSkillInput) (*adminSkillOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := SkillPatch{NameTR: b.NameTR, NameEN: b.NameEN, IsActive: b.IsActive}
	if b.Category != nil {
		p.Category = ptr(SkillCategory(*b.Category))
	}
	k, err := h.svc.AdminUpdateSkill(ctx, a, httpx.ParseID(in.SkillID), p)
	if err != nil {
		return nil, err
	}
	return &adminSkillOutput{Body: toAdminSkillDTO(k)}, nil
}
