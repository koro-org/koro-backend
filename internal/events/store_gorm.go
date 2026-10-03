package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// NewGormStore returns the PostgreSQL store over events, event_forms and
// event_staff (02 §11.1–11.3).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// Column lists (search_vector is never read).
const (
	eventCols = `id, org_id, created_by, title, slug, summary, description_json, description_text, category, format,
		location_name, address, city, online_url, starts_at, ends_at, cover_file_id, visibility, visibility_university_id,
		status, published_at, cancelled_at, cancel_reason, requires_registration, registration_mode, capacity,
		confirmed_count, waitlist_enabled, registration_opens_at, registration_closes_at, cancel_deadline_at,
		checkin_enabled, attendance_finalized_at, attendance_finalized_by, is_sensitive_category, created_at, updated_at`
	staffCols = `event_id, user_id, org_id, role, invited_by, created_at`
)

// constraints maps the named constraints of 02 §11.1–11.4 onto module
// errors. Unlisted CHECKs are 422 and FKs/uniques 409 (db.Translate).
var constraints = db.Constraints{
	"events_org_slug_uq":                   ErrSlugTaken,
	"event_staff_pkey":                     ErrStaffExists,
	"events_org_id_fkey":                   problem.ErrNotFound,
	"event_staff_user_id_fkey":             problem.ErrNotFound,
	"events_cover_file_id_fkey":            problem.ErrValidation,
	"events_visibility_university_id_fkey": problem.ErrValidation,
	// A draft whose form already has a published version (BE-026) keeps it:
	// the versions are immutable, so the draft cannot be deleted.
	"event_form_versions_form_id_fkey":    ErrInvalidState,
	"event_form_versions_event_id_fkey":   ErrInvalidState,
	"event_forms_current_version_id_fkey": ErrInvalidState,
}

func wrap(op string, err error) error {
	return fmt.Errorf("events: %s: %w", op, constraints.Translate(err))
}

// take reads one row of q into T or ErrNotFound.
func take[T any](op, cols string, q *gorm.DB) (T, error) {
	var v T
	err := q.Select(cols).Take(&v).Error
	switch {
	case db.IsNotFound(err):
		return v, ErrNotFound
	case err != nil:
		return v, wrap(op, err)
	}
	return v, nil
}

// returning runs a raw INSERT/UPDATE … RETURNING and returns its row or
// ErrNotFound when nothing was written.
func returning[T any](ctx context.Context, orm *gorm.DB, op, sql string, args map[string]any) (T, error) {
	var rows []T
	if err := db.Conn(ctx, orm).Raw(sql, args).Scan(&rows).Error; err != nil {
		var zero T
		return zero, wrap(op, err)
	}
	if len(rows) == 0 {
		var zero T
		return zero, ErrNotFound
	}
	return rows[0], nil
}

func locked(q *gorm.DB) *gorm.DB {
	return q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
}

// eventArgs are the named arguments of the insert and update statements.
func eventArgs(e Event) map[string]any {
	return map[string]any{
		"id": e.ID, "org_id": e.OrgID, "created_by": e.CreatedBy, "title": e.Title, "slug": e.Slug, "summary": e.Summary,
		"description_json": e.DescriptionJSON, "description_text": e.DescriptionText, "category": string(e.Category),
		"format": string(e.Format), "location_name": e.LocationName, "address": e.Address, "city": e.City,
		"online_url": e.OnlineURL, "starts_at": e.StartsAt, "ends_at": e.EndsAt, "cover_file_id": e.CoverFileID,
		"visibility": string(e.Visibility), "visibility_university_id": e.VisibilityUniversityID,
		"status": string(e.Status), "published_at": e.PublishedAt, "cancelled_at": e.CancelledAt,
		"cancel_reason": e.CancelReason, "requires_registration": e.RequiresRegistration,
		"registration_mode": string(e.RegistrationMode), "capacity": e.Capacity, "waitlist_enabled": e.WaitlistEnabled,
		"registration_opens_at": e.RegistrationOpensAt, "registration_closes_at": e.RegistrationClosesAt,
		"cancel_deadline_at": e.CancelDeadlineAt, "checkin_enabled": e.CheckinEnabled,
		"is_sensitive_category": e.IsSensitiveCategory,
	}
}

// InsertEvent implements Store.
//
// cross-org: a new event of the org the service authorised (authz.Can on
// the org's management surface); the row is the root of its own scope.
func (s gormStore) InsertEvent(ctx context.Context, e Event) (Event, bool, error) {
	if e.ID == uuid.Nil() {
		return Event{}, false, fmt.Errorf("events: insert event: %w", problem.ErrInternal)
	}
	out, err := returning[Event](ctx, s.orm, "insert event", `INSERT INTO events
		(id, org_id, created_by, title, slug, summary, description_json, description_text, category, format,
		 location_name, address, city, online_url, starts_at, ends_at, cover_file_id, visibility,
		 visibility_university_id, status, published_at, cancelled_at, cancel_reason, requires_registration,
		 registration_mode, capacity, waitlist_enabled, registration_opens_at, registration_closes_at,
		 cancel_deadline_at, checkin_enabled, is_sensitive_category)
		VALUES ( @id, @org_id, @created_by, @title, @slug, @summary, @description_json, @description_text, @category,
		 @format, @location_name, @address, @city, @online_url, @starts_at, @ends_at, @cover_file_id, @visibility,
		 @visibility_university_id, @status, @published_at, @cancelled_at, @cancel_reason, @requires_registration,
		 @registration_mode, @capacity, @waitlist_enabled, @registration_opens_at, @registration_closes_at,
		 @cancel_deadline_at, @checkin_enabled, @is_sensitive_category)
		ON CONFLICT ON CONSTRAINT events_org_slug_uq DO NOTHING
		RETURNING `+eventCols, eventArgs(e))
	switch {
	case errors.Is(err, ErrNotFound):
		return Event{}, false, nil // the slug is taken
	case err != nil:
		return Event{}, false, err
	}
	return out, true, nil
}

// GetEvent implements Store.
//
// cross-org: an event is addressed by its id; the service takes org_id
// from the row (06 H-38) and authz.Can decides its visibility first.
func (s gormStore) GetEvent(ctx context.Context, id uuid.UUID) (Event, error) {
	return take[Event]("get event", eventCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// LockEvent implements Store.
//
// cross-org: as GetEvent, with the row locked.
func (s gormStore) LockEvent(ctx context.Context, id uuid.UUID) (Event, error) {
	return take[Event]("lock event", eventCols, locked(db.Conn(ctx, s.orm)).Where("id = ?", id))
}

// UpdateEvent implements Store.
func (s gormStore) UpdateEvent(ctx context.Context, e Event) (Event, error) {
	return returning[Event](ctx, s.orm, "update event", `UPDATE events SET
		title = @title, slug = @slug, summary = @summary, description_json = @description_json,
		description_text = @description_text, category = @category, format = @format, location_name = @location_name,
		address = @address, city = @city, online_url = @online_url, starts_at = @starts_at, ends_at = @ends_at,
		cover_file_id = @cover_file_id, visibility = @visibility, visibility_university_id = @visibility_university_id,
		status = @status, published_at = @published_at, cancelled_at = @cancelled_at, cancel_reason = @cancel_reason,
		requires_registration = @requires_registration, registration_mode = @registration_mode, capacity = @capacity,
		waitlist_enabled = @waitlist_enabled, registration_opens_at = @registration_opens_at,
		registration_closes_at = @registration_closes_at, cancel_deadline_at = @cancel_deadline_at,
		checkin_enabled = @checkin_enabled, is_sensitive_category = @is_sensitive_category
		WHERE id = @id AND org_id = @org_id RETURNING `+eventCols, eventArgs(e))
}

// DeleteDraft implements Store. The children go first (FKs without
// cascade, 02 §0.1); the BEFORE DELETE trigger keeps every other status.
func (s gormStore) DeleteDraft(ctx context.Context, orgID, id uuid.UUID) error {
	conn := db.Conn(ctx, s.orm)
	args := map[string]any{"id": id, "org_id": orgID}
	var n int64
	if err := conn.Raw(`SELECT count(*) FROM events WHERE id = @id AND org_id = @org_id AND status = 'draft'`, args).
		Scan(&n).Error; err != nil {
		return wrap("delete draft", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	for _, stmt := range []string{
		`DELETE FROM event_staff WHERE event_id = @id AND org_id = @org_id`,
		`DELETE FROM event_forms WHERE event_id = @id AND org_id = @org_id`,
		`DELETE FROM events WHERE id = @id AND org_id = @org_id AND status = 'draft'`,
	} {
		if err := conn.Exec(stmt, args).Error; err != nil {
			return wrap("delete draft", err)
		}
	}
	return nil
}

// ListEvents implements Store.
//
// cross-org: the public event list spans every organiser; the viewer's
// visibility (03 §3.6) is applied in the query.
func (s gormStore) ListEvents(ctx context.Context, f ListFilter, after *After, limit int) ([]Event, error) {
	q := db.Conn(ctx, s.orm).Model(&Event{}).Select(eventCols).Where("status = ?", string(StatusPublished))
	if !f.Viewer.All {
		conds, args := []string{"visibility = 'public'"}, []any{}
		if v := f.Viewer.UniversityID; v != nil {
			conds, args = append(conds, "(visibility = 'university' AND visibility_university_id = ?)"), append(args, *v)
		}
		if len(f.Viewer.MemberOrgs) > 0 {
			conds, args = append(conds, "org_id IN ?"), append(args, f.Viewer.MemberOrgs)
		}
		if v := f.Viewer.UserID; v != nil {
			conds = append(conds, "EXISTS (SELECT 1 FROM event_staff s WHERE s.event_id = events.id AND s.user_id = ?)")
			args = append(args, *v)
		}
		q = q.Where("("+strings.Join(conds, " OR ")+")", args...)
	}
	if f.OrgIDs != nil {
		if len(f.OrgIDs) == 0 {
			return []Event{}, nil
		}
		q = q.Where("org_id IN ?", f.OrgIDs)
	}
	if f.Category != "" {
		q = q.Where("category = ?", string(f.Category))
	}
	if f.Format != "" {
		q = q.Where("format = ?", string(f.Format))
	}
	if f.City != "" {
		q = q.Where("public.koro_norm(city) = public.koro_norm( ?)", f.City)
	}
	if f.StartsFrom != nil {
		q = q.Where("starts_at >= ?", *f.StartsFrom)
	}
	if f.StartsTo != nil {
		q = q.Where("starts_at < ?", *f.StartsTo)
	}
	if f.EndsAfter != nil {
		q = q.Where("ends_at > ?", *f.EndsAfter)
	}
	if f.Q != "" {
		// BE-020's Turkish pattern (02 §0.3, 05 §9.5): full text with
		// koro_tr, or a substring / trigram match of koro_norm(title).
		q = q.Where(`(search_vector @@ plainto_tsquery('public.koro_tr', ?)
			OR public.koro_norm(title) LIKE '%' || public.koro_norm( ?) || '%'
			OR public.koro_norm(title) OPERATOR(public.%>) public.koro_norm( ?))`,
			f.Q, textx.EscapeLike(f.Q), f.Q)
	}
	if f.Sort == SortPublishedNew {
		if after != nil {
			q = q.Where("(published_at, id) < ( ?, ?)", after.At, after.ID)
		}
		q = q.Order("published_at DESC, id DESC")
	} else {
		if after != nil {
			q = q.Where("(starts_at, id) > ( ?, ?)", after.At, after.ID)
		}
		q = q.Order("starts_at, id")
	}
	var out []Event
	if err := q.Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list events", err)
	}
	return out, nil
}

// ListOrgEvents implements Store.
func (s gormStore) ListOrgEvents(ctx context.Context, orgID uuid.UUID, f OrgListFilter, after *After, limit int) ([]Event, error) {
	q := db.Conn(ctx, s.orm).Model(&Event{}).Select(eventCols).Scopes(db.ScopeOrg(orgID))
	if len(f.Statuses) > 0 {
		st := make([]string, len(f.Statuses))
		for i, x := range f.Statuses {
			st[i] = string(x)
		}
		q = q.Where("status IN ?", st)
	}
	if after != nil {
		q = q.Where("(starts_at, id) > ( ?, ?)", after.At, after.ID)
	}
	var out []Event
	if err := q.Order("starts_at, id").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list org events", err)
	}
	return out, nil
}

// InsertForm implements Store. org_id comes from the events row (06 H-38).
func (s gormStore) InsertForm(ctx context.Context, orgID uuid.UUID, f Form) error {
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO event_forms (id, event_id, org_id, draft_schema, updated_by)
		SELECT @id, e.id, e.org_id, @draft_schema, @updated_by FROM events e WHERE e.id = @event_id AND e.org_id = @org_id`,
		map[string]any{"id": f.ID, "event_id": f.EventID, "org_id": orgID, "draft_schema": f.DraftSchema, "updated_by": f.UpdatedBy})
	if res.Error != nil {
		return wrap("insert form", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// HasPublishedForm implements Store.
func (s gormStore) HasPublishedForm(ctx context.Context, orgID, id uuid.UUID) (bool, error) {
	var n int64
	err := db.Conn(ctx, s.orm).Model(&Form{}).Scopes(db.ScopeOrg(orgID)).
		Where("event_id = ? AND current_version_id IS NOT NULL", id).Count(&n).Error
	if err != nil {
		return false, wrap("has published form", err)
	}
	return n > 0, nil
}

// StaffRole implements Store.
func (s gormStore) StaffRole(ctx context.Context, orgID, eventID, userID uuid.UUID) (StaffRole, bool, error) {
	st, err := take[Staff]("staff role", staffCols, db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).
		Where("event_id = ? AND user_id = ?", eventID, userID))
	switch {
	case errors.Is(err, ErrNotFound):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return st.Role, true, nil
}

// ListStaff implements Store.
func (s gormStore) ListStaff(ctx context.Context, orgID, eventID uuid.UUID) ([]Staff, error) {
	var out []Staff
	err := db.Conn(ctx, s.orm).Model(&Staff{}).Select(staffCols).Scopes(db.ScopeOrg(orgID)).
		Where("event_id = ?", eventID).Order("created_at, user_id").Find(&out).Error
	if err != nil {
		return nil, wrap("list staff", err)
	}
	return out, nil
}

// InsertStaff implements Store. org_id comes from the events row (06 H-38).
func (s gormStore) InsertStaff(ctx context.Context, st Staff) (Staff, error) {
	return returning[Staff](ctx, s.orm, "insert staff", `INSERT INTO event_staff (event_id, user_id, org_id, role, invited_by)
		SELECT e.id, @user_id, e.org_id, @role, @invited_by FROM events e WHERE e.id = @event_id AND e.org_id = @org_id
		RETURNING `+staffCols, map[string]any{
		"event_id": st.EventID, "user_id": st.UserID, "org_id": st.OrgID, "role": string(st.Role), "invited_by": st.InvitedBy,
	})
}

// UpdateStaffRole implements Store.
func (s gormStore) UpdateStaffRole(ctx context.Context, orgID, eventID, userID uuid.UUID, role StaffRole) (Staff, error) {
	return returning[Staff](ctx, s.orm, "update staff", `UPDATE event_staff SET role = @role
		WHERE event_id = @event_id AND user_id = @user_id AND org_id = @org_id RETURNING `+staffCols, map[string]any{
		"event_id": eventID, "user_id": userID, "org_id": orgID, "role": string(role),
	})
}

// DeleteStaff implements Store.
func (s gormStore) DeleteStaff(ctx context.Context, orgID, eventID, userID uuid.UUID) error {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM event_staff WHERE event_id = @event_id AND user_id = @user_id
		AND org_id = @org_id`, map[string]any{"event_id": eventID, "user_id": userID, "org_id": orgID})
	if res.Error != nil {
		return wrap("delete staff", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// StaffOfUser implements Store.
//
// cross-org: the data subject's own rows in every org (KVKK export).
func (s gormStore) StaffOfUser(ctx context.Context, userID uuid.UUID) ([]Staff, error) {
	var out []Staff
	err := db.Conn(ctx, s.orm).Model(&Staff{}).Select(staffCols).Where("user_id = ?", userID).
		Order("created_at, event_id").Find(&out).Error
	if err != nil {
		return nil, wrap("staff of user", err)
	}
	return out, nil
}

// DeleteStaffOfUser implements Store.
//
// cross-org: the data subject's own rows in every org (KVKK erasure).
func (s gormStore) DeleteStaffOfUser(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM event_staff WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, wrap("delete staff of user", res.Error)
	}
	return int(res.RowsAffected), nil
}
