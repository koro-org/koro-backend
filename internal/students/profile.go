package students

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/platform/civil"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// ProfilePatch is updateMyStudentProfile (04 §2.4). It has no university:
// the university comes from the student verification and is immutable
// through the API (an unknown body member is 422).
type ProfilePatch struct {
	Department              *uuid.UUID // a department of the student's university
	ClearDepartment         bool
	DegreeLevel             *DegreeLevel
	ClassYear               *int16
	ExpectedGraduation      *civil.Date // first day of the month
	ClearExpectedGraduation bool
	Headline                *string // "" clears
	About                   *string // "" clears
	City                    *string // "" clears
	Avatar                  *uuid.UUID
	ClearAvatar             bool
}

func (p ProfilePatch) empty() bool {
	return p.Department == nil && !p.ClearDepartment && p.DegreeLevel == nil && p.ClassYear == nil &&
		p.ExpectedGraduation == nil && !p.ClearExpectedGraduation && p.Headline == nil && p.About == nil &&
		p.City == nil && p.Avatar == nil && !p.ClearAvatar
}

// SkillInput is one item of putMySkills.
type SkillInput struct {
	SkillID uuid.UUID
	Level   *int16 // 1–5
}

// LanguageInput is one item of putMyLanguages.
type LanguageInput struct {
	LanguageCode string // ISO 639 (2–3 lower-case letters)
	Level        LanguageLevel
}

// ExperienceInput is createMyExperience.
type ExperienceInput struct {
	Kind             ExperienceKind
	Title            string
	OrganizationName string
	StartMonth       civil.Date
	EndMonth         *civil.Date
	Description      string
}

// ExperiencePatch is updateMyExperience (nil: unchanged; "" clears the
// optional texts).
type ExperiencePatch struct {
	Kind             *ExperienceKind
	Title            *string
	OrganizationName *string
	StartMonth       *civil.Date
	EndMonth         *civil.Date
	ClearEndMonth    bool
	Description      *string
}

func (p ExperiencePatch) empty() bool {
	return p.Kind == nil && p.Title == nil && p.OrganizationName == nil && p.StartMonth == nil && p.EndMonth == nil &&
		!p.ClearEndMonth && p.Description == nil
}

// ---- reads

// GetMyProfile implements Service. A verified student who never wrote the
// profile gets the profile it would be created with (university from the
// verification); nothing is written by a read.
func (s *service) GetMyProfile(ctx context.Context, a authz.Actor) (ProfileView, error) {
	if err := s.can(ctx, a, authz.StudentsProfileReadOwn); err != nil {
		return ProfileView{}, err
	}
	p, err := s.d.Store.GetProfile(ctx, a.UserID)
	stored := err == nil
	switch {
	case errors.Is(err, ErrNotFound):
		if p, err = newProfile(a); err != nil {
			return ProfileView{}, err
		}
	case err != nil:
		return ProfileView{}, err
	}
	return s.view(ctx, p, stored)
}

// newProfile is the profile a verified student starts with.
func newProfile(a authz.Actor) (Profile, error) {
	if a.UniversityID == nil {
		return Profile{}, ErrVerificationRequired
	}
	return Profile{UserID: a.UserID, UniversityID: *a.UniversityID, DegreeLevel: DegreeBachelor, Status: ProfileActive}, nil
}

// view loads the sections, the settings and the display facts of p.
func (s *service) view(ctx context.Context, p Profile, stored bool) (ProfileView, error) {
	v := ProfileView{Profile: p, Stored: stored, Privacy: defaultPrivacy(p.UserID)}
	u, err := s.d.Catalog.GetUniversity(ctx, p.UniversityID)
	switch {
	case err == nil:
		v.UniversityName = u.Name
	case !errors.Is(err, directory.ErrNotFound):
		return ProfileView{}, err
	}
	if p.DepartmentID != nil {
		d, err := s.d.Catalog.GetDepartment(ctx, *p.DepartmentID)
		switch {
		case err == nil:
			v.DepartmentName = d.Name
		case !errors.Is(err, directory.ErrNotFound):
			return ProfileView{}, err
		}
	}
	v.AvatarURL = s.avatarURL(ctx, p.UserID, p.AvatarFileID)
	if v.Skills, err = s.d.Store.Skills(ctx, p.UserID); err != nil {
		return ProfileView{}, err
	}
	if v.Experiences, err = s.d.Store.Experiences(ctx, p.UserID); err != nil {
		return ProfileView{}, err
	}
	if v.Languages, err = s.d.Store.Languages(ctx, p.UserID); err != nil {
		return ProfileView{}, err
	}
	switch pr, err := s.d.Store.GetPrivacy(ctx, p.UserID); {
	case err == nil:
		v.Privacy = pr
	case !errors.Is(err, ErrNotFound):
		return ProfileView{}, err
	}
	cvs, err := s.d.Store.CVs(ctx, p.UserID)
	if err != nil {
		return ProfileView{}, err
	}
	if v.CVs, err = s.cvViews(ctx, cvs); err != nil {
		return ProfileView{}, err
	}
	return v, nil
}

// avatarURL returns a short-lived URL of the avatar, or "" (no avatar,
// files not wired, scan not clean): a missing avatar never fails a read.
func (s *service) avatarURL(ctx context.Context, userID uuid.UUID, fileID *uuid.UUID) string {
	if fileID == nil || s.d.Files == nil {
		return ""
	}
	dl, err := s.d.Files.PresignGet(ctx, *fileID, avatarTTL)
	if err != nil {
		if !errors.Is(err, files.ErrNotClean) && !errors.Is(err, files.ErrNotFound) {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "student avatar URL not signed",
				slog.String("user_id", userID.String()), slog.String("err", err.Error()))
		}
		return ""
	}
	return dl.URL
}

// AvatarURLs implements Service.
func (s *service) AvatarURLs(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if s.d.Files == nil || len(userIDs) == 0 {
		return out, nil
	}
	ids, err := s.d.Store.AvatarIDs(ctx, userIDs)
	if err != nil {
		return nil, err
	}
	for u, f := range ids {
		if url := s.avatarURL(ctx, u, &f); url != "" {
			out[u] = url
		}
	}
	return out, nil
}

// ---- profile writes

// ensureProfile returns the caller's profile locked for the transaction,
// creating it on the first write (with its audience); a profile whose
// university differs from the actor's active verification follows the
// verification (02 §9.1: the university comes from it) and loses its
// department.
func (s *service) ensureProfile(ctx context.Context, a authz.Actor) (Profile, error) {
	fresh, err := newProfile(a)
	if err != nil {
		return Profile{}, err
	}
	p, err := s.d.Store.LockProfile(ctx, a.UserID)
	switch {
	case errors.Is(err, ErrNotFound):
		if fresh, err = s.withAudience(ctx, fresh); err != nil {
			return Profile{}, err
		}
		if _, _, err := s.d.Store.InsertProfile(ctx, fresh); err != nil {
			return Profile{}, err
		}
		return s.d.Store.LockProfile(ctx, a.UserID) // ours, or a concurrent first write's
	case err != nil:
		return Profile{}, err
	}
	if p.UniversityID != fresh.UniversityID {
		next := cloneProfile(p)
		next.UniversityID, next.DepartmentID = fresh.UniversityID, nil
		next.CompletedSections = next.withSection(SectionBasics, false)
		if next, err = s.withAudience(ctx, next); err != nil {
			return Profile{}, err
		}
		if p, err = s.d.Store.UpdateProfile(ctx, next); err != nil {
			return Profile{}, err
		}
		if err := s.d.Audit.Log(ctx, writeEvent(a, authz.StudentsProfileUpdateOwn,
			map[string]any{"fields": []string{"university_id"}, "reason": "verification"})); err != nil {
			return Profile{}, err
		}
	}
	return p, nil
}

// UpdateMyProfile implements Service. Order: authz (k-uv 403) → empty
// patch (422) → in one transaction: the profile (created on the first
// write), the patch (department of the student's university, 422
// students.unknown_department), the basics section, the audience keys,
// the replaced avatar's deletion and the audit record.
func (s *service) UpdateMyProfile(ctx context.Context, a authz.Actor, patch ProfilePatch) (ProfileView, error) {
	if err := s.can(ctx, a, authz.StudentsProfileUpdateOwn); err != nil {
		return ProfileView{}, err
	}
	if patch.empty() {
		return ProfileView{}, ErrEmptyPatch
	}
	var out Profile
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.ensureProfile(ctx, a)
		if err != nil {
			return err
		}
		next, changed, err := s.apply(ctx, a, p, patch)
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			out = p
			return nil
		}
		basics := next.DepartmentID != nil && next.ClassYear != nil && next.ExpectedGraduation != nil
		next.CompletedSections = next.withSection(SectionBasics, basics)
		if next, err = s.withAudience(ctx, next); err != nil {
			return err
		}
		if out, err = s.d.Store.UpdateProfile(ctx, next); err != nil {
			return err
		}
		if basics && !p.Completed(SectionBasics) {
			s.sectionCompleted(ctx, a.UserID, SectionBasics)
		}
		if old := p.AvatarFileID; old != nil && (next.AvatarFileID == nil || *next.AvatarFileID != *old) {
			if err := s.removeFile(ctx, a, *old); err != nil {
				return err
			}
		}
		return s.d.Audit.Log(ctx, writeEvent(a, authz.StudentsProfileUpdateOwn, map[string]any{"fields": changed}))
	})
	if err != nil {
		return ProfileView{}, err
	}
	return s.view(ctx, out, true)
}

// apply validates patch against p and returns the changed profile with the
// names of the changed columns.
func (s *service) apply(ctx context.Context, a authz.Actor, p Profile, patch ProfilePatch) (Profile, []string, error) {
	next, changed := p, []string{}
	set := func(name string) { changed = append(changed, name) }
	switch {
	case patch.Department != nil:
		d, err := s.d.Catalog.GetDepartment(ctx, *patch.Department)
		switch {
		case errors.Is(err, directory.ErrNotFound):
			return p, nil, ErrUnknownDepartment.WithFields(departmentField)
		case err != nil:
			return p, nil, err
		case !d.IsActive || d.UniversityID != p.UniversityID:
			return p, nil, ErrUnknownDepartment.WithFields(departmentField)
		}
		if p.DepartmentID == nil || *p.DepartmentID != d.ID {
			id := d.ID
			next.DepartmentID = &id
			set("department_id")
		}
		if patch.DegreeLevel == nil && next.DegreeLevel != DegreeLevel(d.DegreeLevel) && DegreeLevel(d.DegreeLevel).Valid() {
			next.DegreeLevel = DegreeLevel(d.DegreeLevel)
			set("degree_level")
		}
	case patch.ClearDepartment && p.DepartmentID != nil:
		next.DepartmentID = nil
		set("department_id")
	}
	if patch.DegreeLevel != nil {
		if !patch.DegreeLevel.Valid() {
			return p, nil, fieldErr("body.degree_level", "must be associate, bachelor, master or doctorate")
		}
		if *patch.DegreeLevel != next.DegreeLevel {
			next.DegreeLevel = *patch.DegreeLevel
			set("degree_level")
		}
	}
	if patch.ClassYear != nil {
		if y := *patch.ClassYear; y < 1 || y > 8 {
			return p, nil, fieldErr("body.class_year", "must be 1-8")
		}
		if p.ClassYear == nil || *p.ClassYear != *patch.ClassYear {
			y := *patch.ClassYear
			next.ClassYear = &y
			set("class_year")
		}
	}
	switch {
	case patch.ExpectedGraduation != nil:
		if p.ExpectedGraduation == nil || *p.ExpectedGraduation != *patch.ExpectedGraduation {
			d := *patch.ExpectedGraduation
			next.ExpectedGraduation = &d
			set("expected_graduation")
		}
	case patch.ClearExpectedGraduation && p.ExpectedGraduation != nil:
		next.ExpectedGraduation = nil
		set("expected_graduation")
	}
	for _, f := range []struct {
		name string
		in   *string
		cur  **string
		max  int
		ml   bool
	}{
		{"headline", patch.Headline, &next.Headline, maxHeadline, false},
		{"about", patch.About, &next.About, maxAbout, true},
		{"city", patch.City, &next.City, maxCity, false},
	} {
		if f.in == nil {
			continue
		}
		clean := cleanLine
		if f.ml {
			clean = cleanMultiline
		}
		v, err := clean("body."+f.name, *f.in, f.max)
		if err != nil {
			return p, nil, err
		}
		if !equalPtr(*f.cur, v) {
			*f.cur = v
			set(f.name)
		}
	}
	switch {
	case patch.Avatar != nil:
		if p.AvatarFileID == nil || *p.AvatarFileID != *patch.Avatar {
			if err := s.attachAvatar(ctx, a, *patch.Avatar); err != nil {
				return p, nil, err
			}
			id := *patch.Avatar
			next.AvatarFileID = &id
			set("avatar_file_id")
		}
	case patch.ClearAvatar && p.AvatarFileID != nil:
		next.AvatarFileID = nil
		set("avatar_file_id")
	}
	return next, changed, nil
}

var departmentField = problem.FieldError{Location: "body.department_id", Message: "is not a department of your university"}

func equalPtr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// attachAvatar checks that fileID is the caller's avatar upload and
// attaches it to the profile in the transaction (files k30: the upload was
// authorised for avatar already).
func (s *service) attachAvatar(ctx context.Context, a authz.Actor, fileID uuid.UUID) error {
	if s.d.Files == nil {
		return errUnavailable
	}
	bad := fieldErr("body.avatar_file_id", "must be an avatar upload of yours")
	f, err := s.d.Files.Get(ctx, fileID)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return bad
	case err != nil:
		return err
	}
	if f.Purpose != files.PurposeAvatar || f.UploadedBy == nil || *f.UploadedBy != a.UserID {
		return bad
	}
	return s.d.Files.Attach(ctx, a, fileID, files.Owner{Module: studentsModule, EntityID: a.UserID, Purpose: files.PurposeAvatar})
}

// removeFile deletes an avatar or a CV file students no longer
// references, in the transaction carried by ctx (Files.Delete); without
// files the file stays (logged without personal data).
func (s *service) removeFile(ctx context.Context, a authz.Actor, fileID uuid.UUID) error {
	if s.d.Files == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "file kept: files not wired", slog.String("file_id", fileID.String()))
		return nil
	}
	return s.d.Files.Delete(ctx, a, fileID)
}

// sectionCompleted emits student.profile_section_completed (A) once the
// transaction commits.
func (s *service) sectionCompleted(ctx context.Context, userID uuid.UUID, sec Section) {
	s.emitAfterCommit(ctx, domainevent.StudentProfileSectionCompleted{UserID: userID, Section: string(sec)},
		"student_profile", userID)
}

// markSection records whether sec is complete on p (locked in the
// transaction) and emits the event when it becomes complete.
func (s *service) markSection(ctx context.Context, p Profile, sec Section, done bool) error {
	if p.Completed(sec) == done {
		return nil
	}
	p.CompletedSections = p.withSection(sec, done)
	if _, err := s.d.Store.UpdateProfile(ctx, p); err != nil {
		return err
	}
	if done {
		s.sectionCompleted(ctx, p.UserID, sec)
	}
	return nil
}

// ---- sections

// PutMySkills implements Service.
func (s *service) PutMySkills(ctx context.Context, a authz.Actor, items []SkillInput) ([]Skill, error) {
	if err := s.can(ctx, a, authz.StudentsProfileSectionManage); err != nil {
		return nil, err
	}
	if len(items) > maxSkills {
		return nil, fieldErr("body.items", "at most 30 skills")
	}
	rows := make([]Skill, 0, len(items))
	ids := make([]uuid.UUID, 0, len(items))
	for i, it := range items {
		loc := "body.items[" + strconv.Itoa(i) + "]"
		if slices.Contains(ids, it.SkillID) {
			return nil, fieldErr(loc+".skill_id", "is listed twice")
		}
		if it.Level != nil && (*it.Level < 1 || *it.Level > 5) {
			return nil, fieldErr(loc+".level", "must be 1-5")
		}
		ids = append(ids, it.SkillID)
		rows = append(rows, Skill{UserID: a.UserID, SkillID: it.SkillID, Level: it.Level})
	}
	if err := s.d.Catalog.SkillsExist(ctx, ids); err != nil {
		return nil, err
	}
	var out []Skill
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.ensureProfile(ctx, a)
		if err != nil {
			return err
		}
		if out, err = s.d.Store.ReplaceSkills(ctx, a.UserID, rows); err != nil {
			return err
		}
		if err := s.markSection(ctx, p, SectionSkills, len(out) > 0); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, writeEvent(a, authz.StudentsProfileSectionManage,
			map[string]any{"section": string(SectionSkills), "count": len(out)}))
	})
	return out, err
}

// PutMyLanguages implements Service.
func (s *service) PutMyLanguages(ctx context.Context, a authz.Actor, items []LanguageInput) ([]Language, error) {
	if err := s.can(ctx, a, authz.StudentsProfileSectionManage); err != nil {
		return nil, err
	}
	if len(items) > maxLanguages {
		return nil, fieldErr("body.items", "at most 20 languages")
	}
	rows := make([]Language, 0, len(items))
	for i, it := range items {
		loc := "body.items[" + strconv.Itoa(i) + "]"
		switch {
		case !languageCodeRe.MatchString(it.LanguageCode):
			return nil, fieldErr(loc+".language_code", "must be an ISO 639 code (2-3 lower-case letters)")
		case !it.Level.Valid():
			return nil, fieldErr(loc+".level", "must be A1-C2 or native")
		case slices.ContainsFunc(rows, func(l Language) bool { return l.LanguageCode == it.LanguageCode }):
			return nil, fieldErr(loc+".language_code", "is listed twice")
		}
		rows = append(rows, Language{UserID: a.UserID, LanguageCode: it.LanguageCode, Level: it.Level})
	}
	var out []Language
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.ensureProfile(ctx, a)
		if err != nil {
			return err
		}
		if out, err = s.d.Store.ReplaceLanguages(ctx, a.UserID, rows); err != nil {
			return err
		}
		if err := s.markSection(ctx, p, SectionLanguages, len(out) > 0); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, writeEvent(a, authz.StudentsProfileSectionManage,
			map[string]any{"section": string(SectionLanguages), "count": len(out)}))
	})
	return out, err
}

// cleanExperience validates and normalises an experience.
func cleanExperience(e Experience) (Experience, error) {
	if !e.Kind.Valid() {
		return e, fieldErr("body.kind", "must be internship, job, volunteer, project or club_role")
	}
	title, err := cleanLine("body.title", e.Title, maxTitle)
	if err != nil {
		return e, err
	}
	if title == nil {
		return e, fieldErr("body.title", "is required")
	}
	e.Title = *title
	if e.OrganizationName != nil {
		if e.OrganizationName, err = cleanLine("body.organization_name", *e.OrganizationName, maxOrgName); err != nil {
			return e, err
		}
	}
	if e.Description != nil {
		if e.Description, err = cleanMultiline("body.description", *e.Description, maxDescription); err != nil {
			return e, err
		}
	}
	if e.EndMonth != nil && e.EndMonth.Before(e.StartMonth) {
		return e, fieldErr("body.end_month", "must not be before start_month")
	}
	return e, nil
}

// CreateMyExperience implements Service (optional Idempotency-Key).
func (s *service) CreateMyExperience(ctx context.Context, a authz.Actor, in ExperienceInput) (Experience, error) {
	if err := s.can(ctx, a, authz.StudentsProfileSectionManage); err != nil {
		return Experience{}, err
	}
	e, err := cleanExperience(Experience{
		UserID: a.UserID, Kind: in.Kind, Title: in.Title, OrganizationName: &in.OrganizationName,
		StartMonth: in.StartMonth, EndMonth: in.EndMonth, Description: &in.Description,
	})
	if err != nil {
		return Experience{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op   string
		User uuid.UUID
		In   ExperienceInput
	}{"create_experience", a.UserID, in})
	if err != nil {
		return Experience{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Experience, error) {
		var out Experience
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			p, err := s.ensureProfile(ctx, a)
			if err != nil {
				return err
			}
			cur, err := s.d.Store.Experiences(ctx, a.UserID)
			if err != nil {
				return err
			}
			if len(cur) >= maxExperiences {
				return fieldErr("body", "at most 50 experiences")
			}
			e.ID = uuid.NewV7()
			if out, err = s.d.Store.InsertExperience(ctx, e); err != nil {
				return err
			}
			if err := s.markSection(ctx, p, SectionExperience, true); err != nil {
				return err
			}
			return s.d.Audit.Log(ctx, writeEvent(a, authz.StudentsProfileSectionManage,
				map[string]any{"section": string(SectionExperience), "experience_id": out.ID.String(), "op": "create"}))
		})
		return out, err
	})
	return out, err
}

// UpdateMyExperience implements Service.
func (s *service) UpdateMyExperience(ctx context.Context, a authz.Actor, id uuid.UUID, patch ExperiencePatch) (Experience, error) {
	if err := s.can(ctx, a, authz.StudentsProfileSectionManage); err != nil {
		return Experience{}, err
	}
	var out Experience
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, err := s.d.Store.GetExperience(ctx, a.UserID, id) // another user's ID: 404
		if err != nil {
			return err
		}
		if patch.empty() {
			return ErrEmptyPatch
		}
		if patch.Kind != nil {
			e.Kind = *patch.Kind
		}
		if patch.Title != nil {
			e.Title = *patch.Title
		}
		if patch.OrganizationName != nil {
			e.OrganizationName = patch.OrganizationName
		}
		if patch.StartMonth != nil {
			e.StartMonth = *patch.StartMonth
		}
		switch {
		case patch.EndMonth != nil:
			e.EndMonth = patch.EndMonth
		case patch.ClearEndMonth:
			e.EndMonth = nil
		}
		if patch.Description != nil {
			e.Description = patch.Description
		}
		if e, err = cleanExperience(e); err != nil {
			return err
		}
		if out, err = s.d.Store.UpdateExperience(ctx, e); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, writeEvent(a, authz.StudentsProfileSectionManage,
			map[string]any{"section": string(SectionExperience), "experience_id": id.String(), "op": "update"}))
	})
	return out, err
}

// DeleteMyExperience implements Service.
func (s *service) DeleteMyExperience(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	if err := s.can(ctx, a, authz.StudentsProfileSectionManage); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.d.Store.LockProfile(ctx, a.UserID) // no profile: no experience either (404)
		if err != nil {
			return err
		}
		ok, err := s.d.Store.DeleteExperience(ctx, a.UserID, id) // another user's ID: 404
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		rest, err := s.d.Store.Experiences(ctx, a.UserID)
		if err != nil {
			return err
		}
		if err := s.markSection(ctx, p, SectionExperience, len(rest) > 0); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, writeEvent(a, authz.StudentsProfileSectionManage,
			map[string]any{"section": string(SectionExperience), "experience_id": id.String(), "op": "delete"}))
	})
}
