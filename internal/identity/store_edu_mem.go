package identity

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"
)

// checkEdu mirrors the edu_verifications CHECKs the services rely on
// (02 §5.4; 000021).
func checkEdu(v EduRecord) error {
	email := v.Method == EduMethodEmail
	switch {
	case v.Method != EduMethodEmail && v.Method != EduMethodDocument:
		return fmt.Errorf("%w: edu_verifications_method_chk", errMemCheck)
	case email != (v.EduEmail != "") || email != (v.EduEmailNormalized != "") || email != (v.EmailDomain != ""):
		return fmt.Errorf("%w: edu_verifications_email_method_chk", errMemCheck)
	case !email && v.Status == EduPending && v.DocumentFileID == nil, email && v.DocumentFileID != nil:
		return fmt.Errorf("%w: edu_verifications_document_method_chk", errMemCheck)
	case v.Status == EduVerified && (v.VerifiedAt == nil || v.ExpiresAt == nil || v.UniversityID == nil):
		return fmt.Errorf("%w: edu_verifications_verified_chk", errMemCheck)
	case v.ExpiresAt != nil && (v.VerifiedAt == nil || !v.ExpiresAt.After(*v.VerifiedAt)):
		return fmt.Errorf("%w: edu_verifications_expiry_chk", errMemCheck)
	case !email && (v.Status == EduVerified || v.Status == EduRejected) && v.ReviewedBy == nil:
		return fmt.Errorf("%w: edu_verifications_document_review_chk", errMemCheck)
	case v.ReviewedBy != nil && *v.ReviewedBy == v.UserID:
		return ErrForbidden // edu_verifications_reviewer_chk (03 Y6)
	case len([]rune(v.ReviewNote)) > 500:
		return fmt.Errorf("%w: edu_verifications_review_note_chk", errMemCheck)
	}
	switch v.Status {
	case EduPending, EduVerified, EduRejected, EduExpired, EduRevoked, EduSuperseded:
		return nil
	}
	return fmt.Errorf("%w: edu_verifications_status_chk", errMemCheck)
}

// eduConflict mirrors the partial unique indexes of edu_verifications.
func (m *MemStore) eduConflict(v EduRecord) error {
	for _, o := range m.edu {
		if o.ID == v.ID {
			continue
		}
		switch {
		case v.Status == EduPending && o.Status == EduPending && o.UserID == v.UserID:
			return ErrEduPending
		case v.Status == EduVerified && o.Status == EduVerified && o.UserID == v.UserID:
			return fmt.Errorf("identity memstore: edu_verifications_one_verified_uq: %w", ErrEduInvalidState)
		case v.EduEmailNormalized != "" && o.EduEmailNormalized == v.EduEmailNormalized &&
			(v.Status == EduPending || v.Status == EduVerified) && (o.Status == EduPending || o.Status == EduVerified):
			return ErrEduEmailTaken
		}
	}
	return nil
}

// InsertEduVerification implements Store.
func (m *MemStore) InsertEduVerification(_ context.Context, v EduRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[v.UserID]; !ok {
		return fmt.Errorf("identity memstore: edu verification of unknown user: %w", ErrNotFound)
	}
	if err := checkEdu(v); err != nil {
		return err
	}
	if _, dup := m.edu[v.ID]; dup {
		return fmt.Errorf("identity memstore: duplicate edu verification %s", v.ID)
	}
	if err := m.eduConflict(v); err != nil {
		return err
	}
	m.edu[v.ID] = v.clone()
	return nil
}

// EduVerification implements Store.
func (m *MemStore) EduVerification(_ context.Context, id uuid.UUID) (EduRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.edu[id]
	if !ok {
		return EduRecord{}, ErrNotFound
	}
	return v.clone(), nil
}

// LockEduVerification implements Store (no locking in memory).
func (m *MemStore) LockEduVerification(ctx context.Context, id uuid.UUID) (EduRecord, error) {
	return m.EduVerification(ctx, id)
}

// eduWhere returns the rows matching keep, ordered by id (desc: newest first).
func (m *MemStore) eduWhere(keep func(EduRecord) bool, desc bool) []EduRecord {
	var out []EduRecord
	for _, v := range m.edu {
		if keep(v) {
			out = append(out, v.clone())
		}
	}
	slices.SortFunc(out, func(a, b EduRecord) int {
		if desc {
			return cmp.Compare(b.ID.String(), a.ID.String())
		}
		return cmp.Compare(a.ID.String(), b.ID.String())
	})
	return out
}

// LockOpenEduVerifications implements Store.
func (m *MemStore) LockOpenEduVerifications(_ context.Context, userID uuid.UUID) ([]EduRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.eduWhere(func(v EduRecord) bool {
		return v.UserID == userID && (v.Status == EduPending || v.Status == EduVerified)
	}, false), nil
}

// UpdateEduVerification implements Store.
func (m *MemStore) UpdateEduVerification(_ context.Context, v EduRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.edu[v.ID]
	if !ok {
		return ErrNotFound
	}
	cur.Status, cur.UniversityID, cur.DocumentFileID = v.Status, cloneID(v.UniversityID), cloneID(v.DocumentFileID)
	cur.VerifiedAt, cur.ExpiresAt, cur.ReviewedBy, cur.ReviewNote = cloneTime(v.VerifiedAt), cloneTime(v.ExpiresAt), cloneID(v.ReviewedBy), v.ReviewNote
	if !v.UpdatedAt.IsZero() {
		cur.UpdatedAt = v.UpdatedAt
	}
	if err := checkEdu(cur); err != nil {
		return err
	}
	if err := m.eduConflict(cur); err != nil {
		return err
	}
	m.edu[v.ID] = cur
	return nil
}

// EduVerificationsOf implements Store.
func (m *MemStore) EduVerificationsOf(_ context.Context, userID uuid.UUID) ([]EduRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.eduWhere(func(v EduRecord) bool { return v.UserID == userID }, true), nil
}

// LatestEduOutcome implements Store.
func (m *MemStore) LatestEduOutcome(_ context.Context, userID uuid.UUID) (EduRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.eduWhere(func(v EduRecord) bool {
		return v.UserID == userID && (v.Status == EduVerified || v.Status == EduExpired || v.Status == EduRevoked)
	}, true)
	if len(rows) == 0 {
		return EduRecord{}, false, nil
	}
	return rows[0], true, nil
}

// ActiveEduVerifications implements Store.
func (m *MemStore) ActiveEduVerifications(_ context.Context, userIDs []uuid.UUID, now time.Time) ([]EduRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.eduWhere(func(v EduRecord) bool { return slices.Contains(userIDs, v.UserID) && v.activeAt(now) }, false), nil
}

// OpenEduVerificationOfEmail implements Store.
func (m *MemStore) OpenEduVerificationOfEmail(_ context.Context, normalized string) (EduRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.eduWhere(func(v EduRecord) bool {
		return normalized != "" && v.EduEmailNormalized == normalized && (v.Status == EduPending || v.Status == EduVerified)
	}, false)
	if len(rows) == 0 {
		return EduRecord{}, false, nil
	}
	return rows[0], true, nil
}

// EduVerificationOfFile implements Store.
func (m *MemStore) EduVerificationOfFile(_ context.Context, fileID uuid.UUID) (EduRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.eduWhere(func(v EduRecord) bool { return v.DocumentFileID != nil && *v.DocumentFileID == fileID }, false)
	if len(rows) == 0 {
		return EduRecord{}, false, nil
	}
	return rows[0], true, nil
}

// EduQueue implements Store.
func (m *MemStore) EduQueue(_ context.Context, q EduQuery) ([]EduRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	desc := q.newestFirst()
	rows := m.eduWhere(func(v EduRecord) bool {
		if v.Method != EduMethodDocument || v.Status != q.Status {
			return false
		}
		if q.After == nil {
			return true
		}
		c := cmp.Compare(v.ID.String(), q.After.String())
		return desc && c < 0 || !desc && c > 0
	}, desc)
	return rows[:min(len(rows), max(q.Limit, 0))], nil
}

// ExpireEduVerifications implements Store.
func (m *MemStore) ExpireEduVerifications(_ context.Context, now time.Time, limit int) ([]EduRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.eduWhere(func(v EduRecord) bool {
		return v.Status == EduVerified && v.ExpiresAt != nil && !v.ExpiresAt.After(now)
	}, false)
	rows = rows[:min(len(rows), max(limit, 0))]
	for i := range rows {
		rows[i].Status, rows[i].UpdatedAt = EduExpired, now
		m.edu[rows[i].ID] = rows[i].clone()
	}
	return rows, nil
}

// EduExpiringBetween implements Store.
func (m *MemStore) EduExpiringBetween(_ context.Context, from, to time.Time, after *uuid.UUID, limit int) ([]EduRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.eduWhere(func(v EduRecord) bool {
		return v.Status == EduVerified && v.ExpiresAt != nil && !v.ExpiresAt.Before(from) && v.ExpiresAt.Before(to) &&
			(after == nil || v.ID.String() > after.String())
	}, false)
	return rows[:min(len(rows), max(limit, 0))], nil
}

// DeleteEduVerification implements Store.
func (m *MemStore) DeleteEduVerification(_ context.Context, id uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.edu[id]; !ok {
		return false, nil
	}
	delete(m.edu, id)
	return true, nil
}

// DeleteUserEduVerifications implements Store.
func (m *MemStore) DeleteUserEduVerifications(_ context.Context, userID uuid.UUID) ([]EduRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.eduWhere(func(v EduRecord) bool { return v.UserID == userID }, false)
	for _, v := range rows {
		delete(m.edu, v.ID)
	}
	return rows, nil
}
