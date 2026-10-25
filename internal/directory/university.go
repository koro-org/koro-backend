package directory

import (
	"context"
	"errors"
	"strings"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// UniversityQuery narrows listUniversities (04 §2.3: q, city, type, sort name).
type UniversityQuery struct {
	Q     string
	City  string
	Type  UniversityType
	Sort  string // SortNewest (default) or SortName
	Limit int
	After *After
}

// UniversityPage is a page of universities; Next is nil on the last page.
type UniversityPage struct {
	Items []University
	Next  *After
	Sort  string
}

// UniversityView is getUniversity: the university and its active faculties.
type UniversityView struct {
	University University
	Faculties  []Faculty
}

// DepartmentQuery narrows listDepartments (04 §2.3: q, faculty_id, degree_level).
type DepartmentQuery struct {
	Q           string
	FacultyID   *uuid.UUID
	DegreeLevel DegreeLevel
	Limit       int
	After       *After
}

// DepartmentPage is a page of departments.
type DepartmentPage struct {
	Items []Department
	Next  *After
}

// UniversityInput is the body of adminCreateUniversity.
type UniversityInput struct {
	YokCode    string
	Name       string
	ShortName  string
	City       string
	Type       UniversityType
	WebsiteURL string
}

// UniversityPatch is the body of adminUpdateUniversity: nil leaves a field
// as it is; an empty short name or website clears it.
type UniversityPatch struct {
	Name       *string
	ShortName  *string
	City       *string
	Type       *UniversityType
	WebsiteURL *string
	IsActive   *bool
}

func (p UniversityPatch) empty() bool {
	return p.Name == nil && p.ShortName == nil && p.City == nil && p.Type == nil && p.WebsiteURL == nil && p.IsActive == nil
}

// DomainInput is the body of adminPutUniversityEmailDomain.
type DomainInput struct {
	DomainKind DomainKind
	IsActive   bool
}

// DepartmentInput is the body of adminCreateDepartment.
type DepartmentInput struct {
	FacultyID      *uuid.UUID
	Name           string
	DegreeLevel    DegreeLevel
	DurationYears  *int16
	YokProgramCode string
}

func sortOf(s string) string {
	if s == SortName {
		return SortName
	}
	return SortNewest
}

// ListUniversities implements Service: active universities only.
func (s *service) ListUniversities(ctx context.Context, a authz.Actor, q UniversityQuery) (UniversityPage, error) {
	if err := authz.Can(ctx, a, authz.DirectoryUniversityList, catalogResource); err != nil {
		return UniversityPage{}, err
	}
	text, err := cleanQuery("query.q", q.Q)
	if err != nil {
		return UniversityPage{}, err
	}
	city, err := cleanOptional("query.city", q.City, maxCity)
	if err != nil {
		return UniversityPage{}, err
	}
	f := UniversityFilter{Q: text, City: deref(city), Type: q.Type, Sort: sortOf(q.Sort)}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListUniversities(ctx, f, q.After, limit+1)
	if err != nil {
		return UniversityPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := UniversityPage{Items: rows, Sort: f.Sort}
	if more {
		last := rows[len(rows)-1]
		out.Next = &After{Name: last.Name, ID: last.ID}
	}
	return out, nil
}

// visibleUniversity loads a university and authorises act on it (an
// inactive one is 404 except for platform admins).
func (s *service) visibleUniversity(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) (University, error) {
	u, err := s.d.Store.GetUniversity(ctx, id)
	if err != nil {
		return University{}, err
	}
	if err := authz.Can(ctx, a, act, universityResource(u)); err != nil {
		return University{}, err
	}
	return u, nil
}

// GetUniversityView implements Service.
func (s *service) GetUniversityView(ctx context.Context, a authz.Actor, id uuid.UUID) (UniversityView, error) {
	u, err := s.visibleUniversity(ctx, a, authz.DirectoryUniversityRead, id)
	if err != nil {
		return UniversityView{}, err
	}
	facs, err := s.d.Store.FacultiesOf(ctx, u.ID)
	if err != nil {
		return UniversityView{}, err
	}
	return UniversityView{University: u, Faculties: facs}, nil
}

// ListDepartments implements Service: active departments of a visible
// university.
func (s *service) ListDepartments(ctx context.Context, a authz.Actor, universityID uuid.UUID, q DepartmentQuery) (DepartmentPage, error) {
	u, err := s.visibleUniversity(ctx, a, authz.DirectoryDepartmentList, universityID)
	if err != nil {
		return DepartmentPage{}, err
	}
	text, err := cleanQuery("query.q", q.Q)
	if err != nil {
		return DepartmentPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListDepartments(ctx, DepartmentFilter{
		UniversityID: u.ID, Q: text, FacultyID: q.FacultyID, DegreeLevel: q.DegreeLevel,
	}, q.After, limit+1)
	if err != nil {
		return DepartmentPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := DepartmentPage{Items: rows}
	if more {
		out.Next = &After{ID: rows[len(rows)-1].ID}
	}
	return out, nil
}

// cleanUniversity validates the fields of a new university.
func cleanUniversity(in UniversityInput, prefix string) (University, error) {
	code, err := cleanYokCode(prefix+"yok_code", in.YokCode)
	if err != nil {
		return University{}, err
	}
	name, err := cleanText(prefix+"name", in.Name, 2, maxName)
	if err != nil {
		return University{}, err
	}
	short, err := cleanOptional(prefix+"short_name", in.ShortName, maxShortName)
	if err != nil {
		return University{}, err
	}
	city, err := cleanText(prefix+"city", in.City, 2, maxCity)
	if err != nil {
		return University{}, err
	}
	if !in.Type.Valid() {
		return University{}, problem.Field(prefix+"type", "must be one of state, foundation, other")
	}
	site, err := cleanWebsite(prefix+"website_url", in.WebsiteURL)
	if err != nil {
		return University{}, err
	}
	return University{YokCode: code, Name: name, ShortName: short, City: city, Type: in.Type, WebsiteURL: site, IsActive: true}, nil
}

// orgWebsite is the org profile's website: https only (organizations
// CHECK), so an http university site is not copied.
func orgWebsite(site *string) string {
	if site != nil && strings.HasPrefix(*site, "https://") && httpsURL(*site) {
		return *site
	}
	return ""
}

// createUniversity opens the university's org (orgs.OrgAdmin.Create, in the
// caller's transaction) and writes the universities row.
func (s *service) createUniversity(ctx context.Context, u University, by *uuid.UUID) (University, error) {
	orgID, err := s.d.Orgs.Create(ctx, orgs.CreateOrg{
		Kind: orgs.KindUniversity, Name: u.Name, City: u.City, WebsiteURL: orgWebsite(u.WebsiteURL), CreatedBy: by,
	})
	if err != nil {
		return University{}, err
	}
	u.ID, u.OrgID = uuid.NewV7(), orgID
	return s.d.Store.InsertUniversity(ctx, u)
}

// AdminCreateUniversity implements Service: the university's org and its
// row in one transaction (01 §6.3, T-78).
func (s *service) AdminCreateUniversity(ctx context.Context, a authz.Actor, in UniversityInput) (University, error) {
	if err := authz.Can(ctx, a, authz.DirectoryUniversityManage, adminSurface); err != nil {
		return University{}, err
	}
	u, err := cleanUniversity(in, "body.")
	if err != nil {
		return University{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op string
		U  University
	}{"create_university", u})
	if err != nil {
		return University{}, err
	}
	by := a.UserID
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		if _, err := s.d.Store.UniversityByYokCode(ctx, u.YokCode); err == nil {
			return uuid.Nil(), ErrYokCodeTaken
		} else if !errors.Is(err, ErrNotFound) {
			return uuid.Nil(), err
		}
		created, err := s.createUniversity(ctx, u, &by)
		if err != nil {
			return uuid.Nil(), err
		}
		org := created.OrgID
		return created.ID, s.d.Audit.Log(ctx, auditEvent(a, authz.DirectoryUniversityManage, authz.TypeUniversity, created.ID, &org, "",
			map[string]any{"operation": "create_university", "yok_code": created.YokCode}))
	})
	if err != nil {
		return University{}, err
	}
	return s.d.Store.GetUniversity(ctx, id)
}

// AdminUpdateUniversity implements Service. The org profile (its name and
// website) is the org's own (updateOrg): this changes the directory row.
func (s *service) AdminUpdateUniversity(ctx context.Context, a authz.Actor, id uuid.UUID, p UniversityPatch) (University, error) {
	if err := authz.Can(ctx, a, authz.DirectoryUniversityManage, adminSurface); err != nil {
		return University{}, err
	}
	if p.empty() {
		return University{}, ErrEmptyPatch
	}
	var out University
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.d.Store.LockUniversity(ctx, id)
		if err != nil {
			return err
		}
		var changed []string
		if p.Name != nil {
			if u.Name, err = cleanText("body.name", *p.Name, 2, maxName); err != nil {
				return err
			}
			changed = append(changed, "name")
		}
		if p.ShortName != nil {
			if u.ShortName, err = cleanOptional("body.short_name", *p.ShortName, maxShortName); err != nil {
				return err
			}
			changed = append(changed, "short_name")
		}
		if p.City != nil {
			if u.City, err = cleanText("body.city", *p.City, 2, maxCity); err != nil {
				return err
			}
			changed = append(changed, "city")
		}
		if p.Type != nil {
			if !p.Type.Valid() {
				return problem.Field("body.type", "must be one of state, foundation, other")
			}
			u.Type, changed = *p.Type, append(changed, "type")
		}
		if p.WebsiteURL != nil {
			if u.WebsiteURL, err = cleanWebsite("body.website_url", *p.WebsiteURL); err != nil {
				return err
			}
			changed = append(changed, "website_url")
		}
		if p.IsActive != nil {
			u.IsActive, changed = *p.IsActive, append(changed, "is_active")
		}
		if out, err = s.d.Store.UpdateUniversity(ctx, u); err != nil {
			return err
		}
		org := u.OrgID
		return s.d.Audit.Log(ctx, auditEvent(a, authz.DirectoryUniversityManage, authz.TypeUniversity, u.ID, &org, "",
			map[string]any{"operation": "update_university", "fields": changed}))
	})
	return out, err
}

// AdminPutUniversityEmailDomain implements Service: creates the domain or
// updates its kind and status; a domain of another university is 409.
func (s *service) AdminPutUniversityEmailDomain(ctx context.Context, a authz.Actor, universityID uuid.UUID, domain string, in DomainInput) (EmailDomain, bool, error) {
	if err := authz.Can(ctx, a, authz.DirectoryUniversityManage, adminSurface); err != nil {
		return EmailDomain{}, false, err
	}
	d := normalizeDomain(domain)
	if !domainRe.MatchString(d) || len(d) > 253 {
		return EmailDomain{}, false, problem.Field("path.domain", "must be a .edu.tr domain such as itu.edu.tr")
	}
	if !in.DomainKind.Valid() {
		return EmailDomain{}, false, problem.Field("body.domain_kind", "must be one of student, staff, mixed")
	}
	var out EmailDomain
	created := false
	by := a.UserID
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.d.Store.LockUniversity(ctx, universityID)
		if err != nil {
			return err
		}
		cur, err := s.d.Store.GetDomain(ctx, d)
		switch {
		case errors.Is(err, ErrNotFound):
			created = true
			out, err = s.d.Store.InsertDomain(ctx, EmailDomain{
				Domain: d, UniversityID: u.ID, DomainKind: in.DomainKind, IsActive: in.IsActive, VerifiedBy: &by,
			})
		case err != nil:
		case cur.UniversityID != u.ID:
			err = ErrDomainTaken
		default:
			cur.DomainKind, cur.IsActive, cur.VerifiedBy = in.DomainKind, in.IsActive, &by
			out, err = s.d.Store.UpdateDomain(ctx, cur)
		}
		if err != nil {
			return err
		}
		org := u.OrgID
		return s.d.Audit.Log(ctx, auditEvent(a, authz.DirectoryUniversityManage, authz.TypeUniversity, u.ID, &org, "",
			map[string]any{"operation": "put_email_domain", "domain": d, "domain_kind": string(in.DomainKind),
				"is_active": in.IsActive, "created": created}))
	})
	if err != nil {
		return EmailDomain{}, false, err
	}
	return out, created, nil
}

// AdminCreateDepartment implements Service. A faculty must belong to the
// university of the path (06 H-38: the scope comes from the parent row).
func (s *service) AdminCreateDepartment(ctx context.Context, a authz.Actor, universityID uuid.UUID, in DepartmentInput) (Department, error) {
	if err := authz.Can(ctx, a, authz.DirectoryUniversityManage, adminSurface); err != nil {
		return Department{}, err
	}
	d, err := cleanDepartment(in, "body.")
	if err != nil {
		return Department{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op  string
		Uni uuid.UUID
		D   Department
	}{"create_department", universityID, d})
	if err != nil {
		return Department{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		u, err := s.d.Store.LockUniversity(ctx, universityID)
		if err != nil {
			return uuid.Nil(), err
		}
		if d.FacultyID != nil {
			f, err := s.d.Store.GetFaculty(ctx, *d.FacultyID)
			if errors.Is(err, ErrNotFound) || err == nil && f.UniversityID != u.ID {
				return uuid.Nil(), problem.Field("body.faculty_id", "must be a faculty of this university")
			}
			if err != nil {
				return uuid.Nil(), err
			}
		}
		d.ID, d.UniversityID = uuid.NewV7(), u.ID
		created, err := s.d.Store.InsertDepartment(ctx, d)
		if err != nil {
			return uuid.Nil(), err
		}
		org := u.OrgID
		return created.ID, s.d.Audit.Log(ctx, auditEvent(a, authz.DirectoryUniversityManage, "department", created.ID, &org, "",
			map[string]any{"operation": "create_department", "university_id": u.ID.String()}))
	})
	if err != nil {
		return Department{}, err
	}
	return s.d.Store.GetDepartment(ctx, id)
}

// cleanDepartment validates the fields of a new department.
func cleanDepartment(in DepartmentInput, prefix string) (Department, error) {
	name, err := cleanText(prefix+"name", in.Name, 2, maxName)
	if err != nil {
		return Department{}, err
	}
	if !in.DegreeLevel.Valid() {
		return Department{}, problem.Field(prefix+"degree_level", "must be one of associate, bachelor, master, doctorate")
	}
	if in.DurationYears != nil && (*in.DurationYears < 1 || *in.DurationYears > 7) {
		return Department{}, problem.Field(prefix+"duration_years", "must be 1-7")
	}
	d := Department{FacultyID: in.FacultyID, Name: name, DegreeLevel: in.DegreeLevel, DurationYears: in.DurationYears, IsActive: true}
	if in.YokProgramCode != "" {
		code, err := cleanYokCode(prefix+"yok_program_code", in.YokProgramCode)
		if err != nil {
			return Department{}, err
		}
		d.YokProgramCode = &code
	}
	return d, nil
}
