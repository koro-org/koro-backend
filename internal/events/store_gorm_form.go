package events

import (
	"context"
	"fmt"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// Column lists of the form tables (02 §11.3–11.5).
const (
	formCols    = `id, event_id, org_id, draft_schema, current_version_id, updated_by, created_at, updated_at`
	versionCols = `id, form_id, event_id, version_no, schema, schema_sha256, field_count, has_sensitive_fields,
		lint_report, lint_override_by, lint_override_reason, published_by, published_at`
	versionColsV = `v.id, v.form_id, v.event_id, v.version_no, v.schema, v.schema_sha256, v.field_count,
		v.has_sensitive_fields, v.lint_report, v.lint_override_by, v.lint_override_reason, v.published_by, v.published_at`
	lintCols = `id, class, pattern, description_tr, active, updated_by, created_at, updated_at`
)

// GetForm implements Store.
func (s gormStore) GetForm(ctx context.Context, orgID, eventID uuid.UUID) (Form, error) {
	return take[Form]("get form", formCols, db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Where("event_id = ?", eventID))
}

// LockForm implements Store.
func (s gormStore) LockForm(ctx context.Context, orgID, eventID uuid.UUID) (Form, error) {
	return take[Form]("lock form", formCols, locked(db.Conn(ctx, s.orm)).Scopes(db.ScopeOrg(orgID)).Where("event_id = ?", eventID))
}

// UpdateFormDraft implements Store.
func (s gormStore) UpdateFormDraft(ctx context.Context, orgID, formID uuid.UUID, schema JSON, by uuid.UUID) (Form, error) {
	return returning[Form](ctx, s.orm, "update form draft", `UPDATE event_forms SET draft_schema = @schema, updated_by = @by
		WHERE id = @id AND org_id = @org_id RETURNING `+formCols,
		map[string]any{"schema": schema, "by": by, "id": formID, "org_id": orgID})
}

// InsertFormVersion implements Store: form_id, event_id and the next
// version_no come from the event_forms row of orgID (06 H-38); the caller
// holds the form row lock (LockForm), and the (form_id, version_no) unique
// key is the second layer.
func (s gormStore) InsertFormVersion(ctx context.Context, orgID uuid.UUID, v FormVersion) (FormVersion, error) {
	if v.ID == uuid.Nil() {
		return FormVersion{}, fmt.Errorf("events: insert form version: %w", problem.ErrInternal)
	}
	return returning[FormVersion](ctx, s.orm, "insert form version", `INSERT INTO event_form_versions
		(id, form_id, event_id, version_no, schema, schema_sha256, field_count, has_sensitive_fields, lint_report,
		 lint_override_by, lint_override_reason, published_by, published_at)
		SELECT @id, f.id, f.event_id,
		 COALESCE((SELECT max(x.version_no) FROM event_form_versions x WHERE x.form_id = f.id), 0) + 1,
		 @schema, @sha, @field_count, @sensitive, @lint_report, @override_by, @override_reason, @published_by, @published_at
		FROM event_forms f WHERE f.id = @form_id AND f.org_id = @org_id
		RETURNING `+versionCols, map[string]any{
		"id": v.ID, "form_id": v.FormID, "org_id": orgID, "schema": v.Schema, "sha": v.SchemaSHA256,
		"field_count": v.FieldCount, "sensitive": v.HasSensitiveFields, "lint_report": v.LintReport,
		"override_by": v.LintOverrideBy, "override_reason": v.LintOverrideReason, "published_by": v.PublishedBy,
		"published_at": v.PublishedAt,
	})
}

// SetCurrentVersion implements Store: only a version of the same form.
func (s gormStore) SetCurrentVersion(ctx context.Context, orgID, formID, versionID uuid.UUID) (Form, error) {
	return returning[Form](ctx, s.orm, "set current form version", `UPDATE event_forms f SET current_version_id = @version_id
		WHERE f.id = @form_id AND f.org_id = @org_id
		  AND EXISTS (SELECT 1 FROM event_form_versions v WHERE v.id = @version_id AND v.form_id = f.id)
		RETURNING `+formCols, map[string]any{"version_id": versionID, "form_id": formID, "org_id": orgID})
}

// GetFormVersion implements Store (scoped through the form's org).
func (s gormStore) GetFormVersion(ctx context.Context, orgID, id uuid.UUID) (FormVersion, error) {
	var rows []FormVersion
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+versionColsV+` FROM event_form_versions v
		JOIN event_forms f ON f.id = v.form_id WHERE v.id = @id AND f.org_id = @org_id`,
		map[string]any{"id": id, "org_id": orgID}).Scan(&rows).Error
	switch {
	case err != nil:
		return FormVersion{}, wrap("get form version", err)
	case len(rows) == 0:
		return FormVersion{}, ErrNotFound
	}
	return rows[0], nil
}

// ListFormVersions implements Store.
func (s gormStore) ListFormVersions(ctx context.Context, orgID, formID uuid.UUID) ([]FormVersion, error) {
	var rows []FormVersion
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+versionColsV+` FROM event_form_versions v
		JOIN event_forms f ON f.id = v.form_id WHERE v.form_id = @form_id AND f.org_id = @org_id ORDER BY v.version_no`,
		map[string]any{"form_id": formID, "org_id": orgID}).Scan(&rows).Error
	if err != nil {
		return nil, wrap("list form versions", err)
	}
	return rows, nil
}

// HasRegistrations implements Store.
func (s gormStore) HasRegistrations(ctx context.Context, orgID, eventID uuid.UUID) (bool, error) {
	var ok bool
	err := db.Conn(ctx, s.orm).Raw(`SELECT EXISTS (SELECT 1 FROM event_registrations
		WHERE event_id = @event_id AND org_id = @org_id)`, map[string]any{"event_id": eventID, "org_id": orgID}).
		Scan(&ok).Error
	if err != nil {
		return false, wrap("has registrations", err)
	}
	return ok, nil
}

// ListLintRules implements Store.
//
// cross-org: the lint rules are platform data without an org (02 §11.5).
func (s gormStore) ListLintRules(ctx context.Context) ([]LintRule, error) {
	var out []LintRule
	err := db.Conn(ctx, s.orm).Model(&LintRule{}).Select(lintCols).
		Order("CASE class WHEN 'blocked' THEN 0 WHEN 'warn_special' THEN 1 ELSE 2 END, created_at, id").Find(&out).Error
	if err != nil {
		return nil, wrap("list lint rules", err)
	}
	return out, nil
}

// GetLintRule implements Store.
//
// cross-org: platform data without an org.
func (s gormStore) GetLintRule(ctx context.Context, id uuid.UUID) (LintRule, error) {
	return take[LintRule]("get lint rule", lintCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// InsertLintRule implements Store.
//
// cross-org: platform data without an org.
func (s gormStore) InsertLintRule(ctx context.Context, r LintRule) (LintRule, error) {
	if r.ID == uuid.Nil() {
		return LintRule{}, fmt.Errorf("events: insert lint rule: %w", problem.ErrInternal)
	}
	return returning[LintRule](ctx, s.orm, "insert lint rule", `INSERT INTO form_lint_rules
		(id, class, pattern, description_tr, active, updated_by)
		VALUES ( @id, @class, @pattern, @description_tr, @active, @updated_by) RETURNING `+lintCols, lintArgs(r))
}

// UpdateLintRule implements Store.
//
// cross-org: platform data without an org.
func (s gormStore) UpdateLintRule(ctx context.Context, r LintRule) (LintRule, error) {
	return returning[LintRule](ctx, s.orm, "update lint rule", `UPDATE form_lint_rules SET class = @class,
		pattern = @pattern, description_tr = @description_tr, active = @active, updated_by = @updated_by
		WHERE id = @id RETURNING `+lintCols, lintArgs(r))
}

func lintArgs(r LintRule) map[string]any {
	return map[string]any{
		"id": r.ID, "class": string(r.Class), "pattern": r.Pattern, "description_tr": r.DescriptionTR, "active": r.Active,
		"updated_by": r.UpdatedBy,
	}
}
