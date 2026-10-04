package events

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/problem"
)

// The form side of MemStore (02 §11.3–11.5). Versions are written once and
// never changed (the two layers of 000057 have no in-memory counterpart
// beyond "no method changes them"); the CHECKs of 000057 and 000059 are
// enforced so that a service bug fails here too.

// GetForm implements Store.
func (s *MemStore) GetForm(_ context.Context, orgID, eventID uuid.UUID) (Form, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.forms[eventID]
	if !ok || f.OrgID != orgID {
		return Form{}, ErrNotFound
	}
	f.DraftSchema = cloneJSON(f.DraftSchema)
	return f, nil
}

// LockForm implements Store.
func (s *MemStore) LockForm(ctx context.Context, orgID, eventID uuid.UUID) (Form, error) {
	return s.GetForm(ctx, orgID, eventID)
}

func (s *MemStore) formByID(orgID, formID uuid.UUID) (Form, bool) {
	for _, f := range s.forms {
		if f.ID == formID && f.OrgID == orgID {
			return f, true
		}
	}
	return Form{}, false
}

// UpdateFormDraft implements Store.
func (s *MemStore) UpdateFormDraft(_ context.Context, orgID, formID uuid.UUID, schema JSON, by uuid.UUID) (Form, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.formByID(orgID, formID)
	if !ok {
		return Form{}, ErrNotFound
	}
	if !json.Valid(schema) {
		return Form{}, errCheck
	}
	f.DraftSchema, f.UpdatedBy, f.UpdatedAt = cloneJSON(schema), &by, s.stamp(f.UpdatedAt)
	s.forms[f.EventID] = f
	f.DraftSchema = cloneJSON(f.DraftSchema)
	return f, nil
}

// checkVersion applies the CHECKs of event_form_versions (000057).
func checkVersion(v FormVersion) error {
	var rep struct {
		Blocked          *[]json.RawMessage `json:"blocked"`
		WarnSpecial      *[]json.RawMessage `json:"warn_special"`
		ReviewIdentifier *[]json.RawMessage `json:"review_identifier"`
	}
	switch {
	case len(v.SchemaSHA256) != 32, v.FieldCount < 0, v.FieldCount > MaxFormFields, !json.Valid(v.Schema),
		v.PublishedBy == uuid.Nil():
		return errCheck
	case json.Unmarshal(v.LintReport, &rep) != nil, rep.Blocked == nil, rep.WarnSpecial == nil, rep.ReviewIdentifier == nil:
		return errCheck // event_form_versions_lint_report_chk
	case len(*rep.Blocked) > 0:
		return errCheck // event_form_versions_blocked_chk
	case v.HasSensitiveFields != (len(*rep.WarnSpecial) > 0):
		return errCheck // event_form_versions_sensitive_flag_chk
	case (v.LintOverrideBy == nil) != (v.LintOverrideReason == nil):
		return errCheck // event_form_versions_override_chk
	case v.HasSensitiveFields && v.LintOverrideBy == nil:
		return errCheck // event_form_versions_sensitive_override_chk
	case v.LintOverrideReason != nil && (utf8.RuneCountInString(*v.LintOverrideReason) < 10 ||
		utf8.RuneCountInString(*v.LintOverrideReason) > 500):
		return errCheck
	}
	return nil
}

func cloneVersion(v FormVersion) FormVersion {
	v.Schema, v.LintReport = cloneJSON(v.Schema), cloneJSON(v.LintReport)
	v.SchemaSHA256 = bytes.Clone(v.SchemaSHA256)
	return v
}

// InsertFormVersion implements Store.
func (s *MemStore) InsertFormVersion(_ context.Context, orgID uuid.UUID, v FormVersion) (FormVersion, error) {
	return s.insertVersion(orgID, v)
}

func (s *MemStore) insertVersion(orgID uuid.UUID, v FormVersion) (FormVersion, error) {
	if v.ID == uuid.Nil() {
		return FormVersion{}, errors.New("events: insert form version: missing id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.formByID(orgID, v.FormID)
	if !ok {
		return FormVersion{}, ErrNotFound
	}
	if _, dup := s.versions[v.ID]; dup {
		return FormVersion{}, problem.ErrConflict
	}
	v.EventID, v.VersionNo = f.EventID, 1
	for _, x := range s.versions {
		if x.FormID == f.ID && x.VersionNo >= v.VersionNo {
			v.VersionNo = x.VersionNo + 1
		}
	}
	if err := checkVersion(v); err != nil {
		return FormVersion{}, err
	}
	v = cloneVersion(v)
	s.versions[v.ID] = v
	return cloneVersion(v), nil
}

// SetCurrentVersion implements Store.
func (s *MemStore) SetCurrentVersion(_ context.Context, orgID, formID, versionID uuid.UUID) (Form, error) {
	return s.setCurrent(orgID, formID, versionID)
}

func (s *MemStore) setCurrent(orgID, formID, versionID uuid.UUID) (Form, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.formByID(orgID, formID)
	v, known := s.versions[versionID]
	if !ok || !known || v.FormID != f.ID {
		return Form{}, ErrNotFound
	}
	f.CurrentVersionID, f.UpdatedAt = &versionID, s.stamp(f.UpdatedAt)
	s.forms[f.EventID] = f
	f.DraftSchema = cloneJSON(f.DraftSchema)
	return f, nil
}

// GetFormVersion implements Store.
func (s *MemStore) GetFormVersion(_ context.Context, orgID, id uuid.UUID) (FormVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.versions[id]
	if !ok {
		return FormVersion{}, ErrNotFound
	}
	if _, mine := s.formByID(orgID, v.FormID); !mine {
		return FormVersion{}, ErrNotFound
	}
	return cloneVersion(v), nil
}

// ListFormVersions implements Store.
func (s *MemStore) ListFormVersions(_ context.Context, orgID, formID uuid.UUID) ([]FormVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []FormVersion{}
	if _, ok := s.formByID(orgID, formID); !ok {
		return out, nil
	}
	for _, v := range s.versions {
		if v.FormID == formID {
			out = append(out, cloneVersion(v))
		}
	}
	slices.SortFunc(out, func(a, b FormVersion) int { return cmp.Compare(a.VersionNo, b.VersionNo) })
	return out, nil
}

// HasRegistrations implements Store.
func (s *MemStore) HasRegistrations(_ context.Context, orgID, eventID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[eventID]
	return ok && e.OrgID == orgID && s.reg[eventID], nil
}

// checkLintRule applies the CHECKs of form_lint_rules (000059).
func checkLintRule(r LintRule) error {
	if !r.Class.Valid() || r.Pattern == "" || utf8.RuneCountInString(r.Pattern) > 500 || r.DescriptionTR == "" {
		return errCheck
	}
	return nil
}

func lintRank(c LintClass) int {
	switch c {
	case LintBlocked:
		return 0
	case LintWarnSpecial:
		return 1
	}
	return 2
}

// ListLintRules implements Store.
func (s *MemStore) ListLintRules(context.Context) ([]LintRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.rules)
	slices.SortStableFunc(out, func(a, b LintRule) int {
		return cmp.Or(cmp.Compare(lintRank(a.Class), lintRank(b.Class)), a.CreatedAt.Compare(b.CreatedAt),
			compareUUID(a.ID, b.ID))
	})
	return out, nil
}

// GetLintRule implements Store.
func (s *MemStore) GetLintRule(_ context.Context, id uuid.UUID) (LintRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rules {
		if r.ID == id {
			return r, nil
		}
	}
	return LintRule{}, ErrNotFound
}

// InsertLintRule implements Store.
func (s *MemStore) InsertLintRule(_ context.Context, r LintRule) (LintRule, error) {
	if r.ID == uuid.Nil() {
		return LintRule{}, errors.New("events: insert lint rule: missing id")
	}
	if err := checkLintRule(r); err != nil {
		return LintRule{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.rules {
		if x.ID == r.ID {
			return LintRule{}, problem.ErrConflict
		}
	}
	r.CreatedAt = s.stamp(time.Time{})
	r.UpdatedAt = r.CreatedAt
	s.rules = append(s.rules, r)
	return r, nil
}

// UpdateLintRule implements Store.
func (s *MemStore) UpdateLintRule(_ context.Context, r LintRule) (LintRule, error) {
	if err := checkLintRule(r); err != nil {
		return LintRule{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.rules {
		if x.ID == r.ID {
			r.CreatedAt, r.UpdatedAt = x.CreatedAt, s.stamp(x.UpdatedAt)
			s.rules[i] = r
			return r, nil
		}
	}
	return LintRule{}, ErrNotFound
}
