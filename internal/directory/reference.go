package directory

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// Reference data files of `koro seed reference` (db/reference). Every file
// is UTF-8 CSV with a header row; lines starting with '#' are comments
// (source and licence notes at the top of each file).
const (
	FileUniversities = "universities.csv"  // yok_code,name,short_name,city,type,website_url
	FileDomains      = "email_domains.csv" // domain,university_yok_code,domain_kind
	FileFaculties    = "faculties.csv"     // university_yok_code,name,yok_code
	FileDepartments  = "departments.csv"   // university_yok_code,faculty,name,degree_level,duration_years,yok_program_code
	FileSkills       = "skills.csv"        // slug,name_tr,name_en,category
	seedJob          = "directory.seed_reference"
)

// RefUniversity is one row of universities.csv.
type RefUniversity struct {
	YokCode, Name, ShortName, City string
	Type                           UniversityType
	WebsiteURL                     string
}

// RefDomain is one row of email_domains.csv.
type RefDomain struct {
	Domain, UniversityYokCode string
	DomainKind                DomainKind
}

// RefFaculty is one row of faculties.csv.
type RefFaculty struct {
	UniversityYokCode, Name, YokCode string
}

// RefDepartment is one row of departments.csv. Faculty names a faculty of
// the same university ("" for none).
type RefDepartment struct {
	UniversityYokCode, Faculty, Name string
	DegreeLevel                      DegreeLevel
	DurationYears                    *int16
	YokProgramCode                   string
}

// RefSkill is one row of skills.csv.
type RefSkill struct {
	Slug, NameTR, NameEN string
	Category             SkillCategory
}

// ReferenceData is the parsed content of db/reference.
type ReferenceData struct {
	Universities []RefUniversity
	Domains      []RefDomain
	Faculties    []RefFaculty
	Departments  []RefDepartment
	Skills       []RefSkill
}

// Counts tells what an import did to one table.
type Counts struct {
	Created, Updated, Unchanged int
}

func (c *Counts) add(created, updated bool) {
	switch {
	case created:
		c.Created++
	case updated:
		c.Updated++
	default:
		c.Unchanged++
	}
}

// PlaceholderCodePrefix marks the placeholder YÖK codes of the sample
// reference data (T-204: "ORNEK-ODTU" until the pilot list, PS-09, gives
// the real codes).
const PlaceholderCodePrefix = "ORNEK-"

// PlaceholderCodes returns the placeholder YÖK codes the data uses (a
// university, a domain, faculty or department row naming one, a faculty's
// or department's own code), sorted and without duplicates. `koro seed
// reference` refuses data with any in production (06 H-50).
func (d ReferenceData) PlaceholderCodes() []string {
	var out []string
	add := func(codes ...string) {
		for _, c := range codes {
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(c)), PlaceholderCodePrefix) {
				out = append(out, c)
			}
		}
	}
	for _, u := range d.Universities {
		add(u.YokCode)
	}
	for _, r := range d.Domains {
		add(r.UniversityYokCode)
	}
	for _, f := range d.Faculties {
		add(f.UniversityYokCode, f.YokCode)
	}
	for _, r := range d.Departments {
		add(r.UniversityYokCode, r.YokProgramCode)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ImportResult is the outcome of ImportReference per table.
type ImportResult struct {
	Universities, Domains, Faculties, Departments, Skills Counts
}

// ParseReference reads the five reference files of fsys.
func ParseReference(fsys fs.FS) (ReferenceData, error) {
	var d ReferenceData
	err := errors.Join(
		readCSV(fsys, FileUniversities, []string{"yok_code", "name", "short_name", "city", "type", "website_url"},
			func(r []string) {
				d.Universities = append(d.Universities, RefUniversity{r[0], r[1], r[2], r[3], UniversityType(r[4]), r[5]})
			}),
		readCSV(fsys, FileDomains, []string{"domain", "university_yok_code", "domain_kind"},
			func(r []string) { d.Domains = append(d.Domains, RefDomain{r[0], r[1], DomainKind(r[2])}) }),
		readCSV(fsys, FileFaculties, []string{"university_yok_code", "name", "yok_code"},
			func(r []string) { d.Faculties = append(d.Faculties, RefFaculty{r[0], r[1], r[2]}) }),
		readCSV(fsys, FileSkills, []string{"slug", "name_tr", "name_en", "category"},
			func(r []string) { d.Skills = append(d.Skills, RefSkill{r[0], r[1], r[2], SkillCategory(r[3])}) }),
	)
	var depErr error
	derr := readCSV(fsys, FileDepartments, []string{"university_yok_code", "faculty", "name", "degree_level", "duration_years", "yok_program_code"},
		func(r []string) {
			dep := RefDepartment{UniversityYokCode: r[0], Faculty: r[1], Name: r[2], DegreeLevel: DegreeLevel(r[3]), YokProgramCode: r[5]}
			if r[4] != "" {
				n, err := strconv.ParseInt(r[4], 10, 16)
				if err != nil {
					depErr = errors.Join(depErr, fmt.Errorf("%s: duration_years %q: %w", FileDepartments, r[4], err))
					return
				}
				v := int16(n)
				dep.DurationYears = &v
			}
			d.Departments = append(d.Departments, dep)
		})
	if err := errors.Join(err, derr, depErr); err != nil {
		return ReferenceData{}, err
	}
	return d, nil
}

// readCSV reads name from fsys: the header must equal cols; every record is
// trimmed and handed to row.
func readCSV(fsys fs.FS, name string, cols []string, row func([]string)) error {
	f, err := fsys.Open(name)
	if err != nil {
		return fmt.Errorf("directory: reference %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	r := csv.NewReader(f)
	r.Comment, r.FieldsPerRecord, r.ReuseRecord = '#', len(cols), false
	head, err := r.Read()
	if err != nil {
		return fmt.Errorf("directory: reference %s: header: %w", name, err)
	}
	if !slices.Equal(head, cols) {
		return fmt.Errorf("directory: reference %s: header %v, want %v", name, head, cols)
	}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("directory: reference %s: %w", name, err)
		}
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}
		row(rec)
	}
}

// ImportReference implements Service: one transaction, idempotent upserts
// keyed by YÖK code, then exact name (universities), domain, (university, name) (faculties),
// (university, YÖK program code) or (university, faculty, name, degree)
// (departments) and slug (skills). A new university opens its org with
// orgs.OrgAdmin.Create in the same transaction (01 §6.3). Existing rows get
// the file's values, but is_active is never changed (an admin's
// deactivation survives a re-import). One audit record (system actor)
// summarises the run.
func (s *service) ImportReference(ctx context.Context, data ReferenceData) (ImportResult, error) {
	var res ImportResult
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		res = ImportResult{}
		unis := map[string]University{}
		for i, r := range data.Universities {
			u, created, updated, err := s.importUniversity(ctx, r, fmt.Sprintf("%s:%d.", FileUniversities, i+1))
			if err != nil {
				return err
			}
			unis[u.YokCode] = u
			res.Universities.add(created, updated)
		}
		uniOf := func(file string, i int, code string) (University, error) {
			if u, ok := unis[code]; ok {
				return u, nil
			}
			u, err := s.d.Store.UniversityByYokCode(ctx, code)
			if errors.Is(err, ErrNotFound) {
				return University{}, problem.Field(fmt.Sprintf("%s:%d.university_yok_code", file, i+1), "unknown university")
			}
			if err == nil {
				unis[code] = u
			}
			return u, err
		}
		for i, r := range data.Domains {
			u, err := uniOf(FileDomains, i, r.UniversityYokCode)
			if err != nil {
				return err
			}
			created, updated, err := s.importDomain(ctx, u, r, fmt.Sprintf("%s:%d.", FileDomains, i+1))
			if err != nil {
				return err
			}
			res.Domains.add(created, updated)
		}
		for i, r := range data.Faculties {
			u, err := uniOf(FileFaculties, i, r.UniversityYokCode)
			if err != nil {
				return err
			}
			created, updated, err := s.importFaculty(ctx, u, r, fmt.Sprintf("%s:%d.", FileFaculties, i+1))
			if err != nil {
				return err
			}
			res.Faculties.add(created, updated)
		}
		for i, r := range data.Departments {
			u, err := uniOf(FileDepartments, i, r.UniversityYokCode)
			if err != nil {
				return err
			}
			created, updated, err := s.importDepartment(ctx, u, r, fmt.Sprintf("%s:%d.", FileDepartments, i+1))
			if err != nil {
				return err
			}
			res.Departments.add(created, updated)
		}
		for i, r := range data.Skills {
			created, updated, err := s.importSkill(ctx, r, fmt.Sprintf("%s:%d.", FileSkills, i+1))
			if err != nil {
				return err
			}
			res.Skills.add(created, updated)
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.DirectoryUniversityManage), Outcome: audit.Success, SystemJob: seedJob,
			Metadata: map[string]any{"operation": "seed_reference", "universities": res.Universities.Created,
				"domains": res.Domains.Created, "faculties": res.Faculties.Created,
				"departments": res.Departments.Created, "skills": res.Skills.Created,
				"updated": res.Universities.Updated + res.Domains.Updated + res.Faculties.Updated +
					res.Departments.Updated + res.Skills.Updated},
		})
	})
	return res, err
}

func (s *service) importUniversity(ctx context.Context, r RefUniversity, loc string) (University, bool, bool, error) {
	want, err := cleanUniversity(UniversityInput(r), loc)
	if err != nil {
		return University{}, false, false, err
	}
	cur, err := s.d.Store.UniversityByYokCode(ctx, want.YokCode)
	if errors.Is(err, ErrNotFound) {
		// A university imported under another (e.g. a sample) code keeps its
		// row and org: matched by its exact name, it takes the new code.
		cur, err = s.d.Store.UniversityByName(ctx, want.Name)
	}
	switch {
	case errors.Is(err, ErrNotFound):
		u, err := s.createUniversity(ctx, want, nil)
		return u, true, false, err
	case err != nil:
		return University{}, false, false, err
	}
	if cur.YokCode == want.YokCode && cur.Name == want.Name && deref(cur.ShortName) == deref(want.ShortName) && cur.City == want.City &&
		cur.Type == want.Type && deref(cur.WebsiteURL) == deref(want.WebsiteURL) {
		return cur, false, false, nil
	}
	cur.YokCode, cur.Name, cur.ShortName, cur.City, cur.Type, cur.WebsiteURL =
		want.YokCode, want.Name, want.ShortName, want.City, want.Type, want.WebsiteURL
	u, err := s.d.Store.UpdateUniversity(ctx, cur)
	return u, false, true, err
}

func (s *service) importDomain(ctx context.Context, u University, r RefDomain, loc string) (bool, bool, error) {
	d := normalizeDomain(r.Domain)
	if !domainRe.MatchString(d) {
		return false, false, problem.Field(loc+"domain", "must be a .edu.tr domain")
	}
	if !r.DomainKind.Valid() {
		return false, false, problem.Field(loc+"domain_kind", "must be one of student, staff, mixed")
	}
	cur, err := s.d.Store.GetDomain(ctx, d)
	switch {
	case errors.Is(err, ErrNotFound):
		_, err := s.d.Store.InsertDomain(ctx, EmailDomain{Domain: d, UniversityID: u.ID, DomainKind: r.DomainKind, IsActive: true})
		return true, false, err
	case err != nil:
		return false, false, err
	case cur.UniversityID != u.ID:
		return false, false, ErrDomainTaken.WithFields(fieldLoc(loc + "domain"))
	case cur.DomainKind == r.DomainKind:
		return false, false, nil
	}
	cur.DomainKind = r.DomainKind
	_, err = s.d.Store.UpdateDomain(ctx, cur)
	return false, true, err
}

func (s *service) importFaculty(ctx context.Context, u University, r RefFaculty, loc string) (bool, bool, error) {
	name, err := cleanText(loc+"name", r.Name, 2, maxName)
	if err != nil {
		return false, false, err
	}
	var code *string
	if r.YokCode != "" {
		c, err := cleanYokCode(loc+"yok_code", r.YokCode)
		if err != nil {
			return false, false, err
		}
		code = &c
	}
	cur, err := s.d.Store.FacultyByName(ctx, u.ID, name)
	switch {
	case errors.Is(err, ErrNotFound):
		_, err := s.d.Store.InsertFaculty(ctx, Faculty{ID: uuid.NewV7(), UniversityID: u.ID, Name: name, YokCode: code, IsActive: true})
		return true, false, err
	case err != nil:
		return false, false, err
	case deref(cur.YokCode) == deref(code):
		return false, false, nil
	}
	cur.YokCode = code
	_, err = s.d.Store.UpdateFaculty(ctx, cur)
	return false, true, err
}

func (s *service) importDepartment(ctx context.Context, u University, r RefDepartment, loc string) (bool, bool, error) {
	want, err := cleanDepartment(DepartmentInput{
		Name: r.Name, DegreeLevel: r.DegreeLevel, DurationYears: r.DurationYears, YokProgramCode: r.YokProgramCode,
	}, loc)
	if err != nil {
		return false, false, err
	}
	if r.Faculty != "" {
		f, err := s.d.Store.FacultyByName(ctx, u.ID, r.Faculty)
		if errors.Is(err, ErrNotFound) {
			return false, false, problem.Field(loc+"faculty", "unknown faculty of the university")
		}
		if err != nil {
			return false, false, err
		}
		want.FacultyID = &f.ID
	}
	cur, err := s.d.Store.FindDepartment(ctx, DepartmentKey{
		UniversityID: u.ID, YokProgramCode: deref(want.YokProgramCode), FacultyID: want.FacultyID, Name: want.Name,
		DegreeLevel: want.DegreeLevel,
	})
	switch {
	case errors.Is(err, ErrNotFound):
		want.ID, want.UniversityID = uuid.NewV7(), u.ID
		_, err := s.d.Store.InsertDepartment(ctx, want)
		return true, false, err
	case err != nil:
		return false, false, err
	}
	same := cur.Name == want.Name && cur.DegreeLevel == want.DegreeLevel && deref(cur.DurationYears) == deref(want.DurationYears) &&
		deref(cur.FacultyID) == deref(want.FacultyID) && deref(cur.YokProgramCode) == deref(want.YokProgramCode)
	if same {
		return false, false, nil
	}
	cur.Name, cur.DegreeLevel, cur.DurationYears, cur.FacultyID, cur.YokProgramCode =
		want.Name, want.DegreeLevel, want.DurationYears, want.FacultyID, want.YokProgramCode
	_, err = s.d.Store.UpdateDepartment(ctx, cur)
	return false, true, err
}

func (s *service) importSkill(ctx context.Context, r RefSkill, loc string) (bool, bool, error) {
	want, err := cleanSkill(SkillInput(r), loc)
	if err != nil {
		return false, false, err
	}
	cur, err := s.d.Store.SkillBySlug(ctx, want.Slug)
	switch {
	case errors.Is(err, ErrNotFound):
		want.ID = uuid.NewV7()
		_, err := s.d.Store.InsertSkill(ctx, want)
		return true, false, err
	case err != nil:
		return false, false, err
	case cur.NameTR == want.NameTR && deref(cur.NameEN) == deref(want.NameEN) && cur.Category == want.Category:
		return false, false, nil
	}
	cur.NameTR, cur.NameEN, cur.Category = want.NameTR, want.NameEN, want.Category
	_, err = s.d.Store.UpdateSkill(ctx, cur)
	return false, true, err
}

// ImporterDeps are the dependencies of the `koro seed reference` command:
// no outbox, queue or HTTP-only dependency (the import emits no event).
type ImporterDeps struct {
	Store Store
	Tx    db.TxManager
	Audit AuditLogger
	Orgs  Orgs
	Clock clock.Clock
	Log   *slog.Logger
}

// Importer runs ImportReference outside the API process.
type Importer struct{ s *service }

// NewImporter builds the importer of `koro seed reference`.
func NewImporter(d ImporterDeps) (*Importer, error) {
	if d.Store == nil || d.Tx == nil || d.Audit == nil || d.Orgs == nil || d.Clock == nil || d.Log == nil {
		return nil, errors.New("directory: importer dependencies are missing")
	}
	return &Importer{s: &service{d: Deps{Store: d.Store, Tx: d.Tx, Audit: d.Audit, Orgs: d.Orgs, Clock: d.Clock, Log: d.Log}}}, nil
}

// Import is ImportReference.
func (i *Importer) Import(ctx context.Context, data ReferenceData) (ImportResult, error) {
	return i.s.ImportReference(ctx, data)
}
