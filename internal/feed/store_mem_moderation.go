package feed

import (
	"cmp"
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/problem"
)

// The reports, moderation and trust methods of the in-memory store
// (BE-032), with the CHECKs of 02 §12.1, §12.9–12.10.

// reasonCodeRe is moderation_actions_reason_code_chk.
var reasonCodeRe = regexp.MustCompile(`^[a-z_]{2,40}$`)

// checkReport applies the CHECK constraints of content_reports (the
// snapshot ones: 000168).
func checkReport(r Report) error {
	snap := r.PostSnapshot
	switch {
	case !r.TargetType.Valid(), !r.Reason.Valid(), !r.Status.Valid(), runes(r.Details) > 1000,
		runes(r.ResolutionNote) > 1000:
		return errCheck
	case (r.Status == ReportActioned || r.Status == ReportDismissed) != (r.ResolvedAt != nil):
		return errCheck
	case runes(snap.SnapshotTitle) > 140, runes(snap.SnapshotBodyText) > 5000,
		snap.SnapshotOpportunity != nil && !jsonObject(snap.SnapshotOpportunity):
		return errCheck
	case snap.SnapshotAt != nil && r.TargetType != TargetPost: // content_reports_snapshot_target_chk
		return errCheck
	case snap.SnapshotAt == nil && (snap.SnapshotTitle != nil || snap.SnapshotBodyText != nil ||
		snap.SnapshotOpportunity != nil || snap.SnapshotEventID != nil): // content_reports_snapshot_at_chk
		return errCheck
	}
	return nil
}

// jsonObject reports whether j is a JSON object (jsonb_typeof = 'object').
func jsonObject(j JSON) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(j, &m) == nil && m != nil
}

// PutReport stores r as it is (fixtures).
func (s *MemStore) PutReport(r Report) error {
	if err := checkReport(r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports[r.ID] = r.clone()
	return nil
}

// openReportOf returns the open report of reporter on the target.
func (s *MemStore) openReportOf(r Report) (Report, bool) {
	for _, x := range s.reports {
		if x.ReporterUserID == r.ReporterUserID && x.TargetType == r.TargetType && x.TargetID == r.TargetID &&
			x.Status.Unresolved() {
			return x, true
		}
	}
	return Report{}, false
}

// InsertReport implements Store.
func (s *MemStore) InsertReport(_ context.Context, r Report) (Report, bool, error) {
	if r.ID == uuid.Nil() || r.ReporterUserID == uuid.Nil() {
		return Report{}, false, problem.ErrInternal
	}
	r.Status, r.AssignedTo, r.ResolutionNote, r.ResolvedAt = ReportOpen, nil, nil, nil
	r.PostSnapshot = r.PostSnapshot.clone()
	if err := checkReport(r); err != nil {
		return Report{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if open, ok := s.openReportOf(r); ok {
		return open.clone(), false, nil // the first snapshot stays
	}
	if _, dup := s.reports[r.ID]; dup {
		return Report{}, false, problem.ErrConflict
	}
	r.CreatedAt = s.stamp(time.Time{})
	r.UpdatedAt = r.CreatedAt
	s.reports[r.ID] = r
	return r.clone(), true, nil
}

// GetReport implements Store.
func (s *MemStore) GetReport(_ context.Context, id uuid.UUID) (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reports[id]
	if !ok {
		return Report{}, ErrNotFound
	}
	return r.clone(), nil
}

// LockReport implements Store.
func (s *MemStore) LockReport(ctx context.Context, id uuid.UUID) (Report, error) {
	return s.GetReport(ctx, id)
}

// UpdateReport implements Store.
func (s *MemStore) UpdateReport(_ context.Context, r Report) (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.reports[r.ID]
	if !ok {
		return Report{}, ErrNotFound
	}
	next := cur
	next.Status, next.AssignedTo, next.ResolutionNote, next.ResolvedAt = r.Status, r.AssignedTo, r.ResolutionNote, r.ResolvedAt
	if err := checkReport(next); err != nil {
		return Report{}, err
	}
	if next.Status.Unresolved() && !cur.Status.Unresolved() {
		if open, ok := s.openReportOf(next); ok && open.ID != next.ID { // content_reports_open_uq
			return Report{}, problem.ErrConflict
		}
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.reports[r.ID] = next // the snapshot is cur's (content_reports_snapshot_guard)
	return next.clone(), nil
}

// ListReports implements Store.
func (s *MemStore) ListReports(_ context.Context, f ReportFilter, after *After, limit int) ([]Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Report{}
	for _, r := range s.reports {
		if len(f.Statuses) > 0 && !slices.Contains(f.Statuses, r.Status) || f.Reason != "" && r.Reason != f.Reason ||
			f.TargetType != "" && r.TargetType != f.TargetType {
			continue
		}
		if after != nil {
			c := cmp.Or(r.CreatedAt.Compare(after.At), r.ID.Compare(after.ID))
			if f.Newest && c >= 0 || !f.Newest && c <= 0 {
				continue
			}
		}
		out = append(out, r.clone())
	}
	slices.SortFunc(out, func(a, b Report) int {
		c := cmp.Or(a.CreatedAt.Compare(b.CreatedAt), a.ID.Compare(b.ID))
		if f.Newest {
			return -c
		}
		return c
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// OpenReporters implements Store.
func (s *MemStore) OpenReporters(_ context.Context, t TargetType, targetID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[uuid.UUID]bool{}
	for _, r := range s.reports {
		if r.TargetType == t && r.TargetID == targetID && r.Status.Unresolved() {
			seen[r.ReporterUserID] = true
		}
	}
	return len(seen), nil
}

// ReportsOf implements Store.
func (s *MemStore) ReportsOf(_ context.Context, userID uuid.UUID) ([]Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Report{}
	for _, r := range s.reports {
		if r.ReporterUserID == userID {
			out = append(out, r.clone())
		}
	}
	slices.SortFunc(out, func(a, b Report) int { return a.ID.Compare(b.ID) })
	return out, nil
}

// DeleteReportsOf implements Store (report_id of their actions becomes
// NULL, as ON DELETE SET NULL does).
func (s *MemStore) DeleteReportsOf(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.reports {
		if r.ReporterUserID != userID {
			continue
		}
		delete(s.reports, id)
		n++
		for i := range s.actions {
			if a := s.actions[i].ReportID; a != nil && *a == id {
				s.actions[i].ReportID = nil
			}
		}
	}
	return n, nil
}

// InsertModerationAction implements Store.
func (s *MemStore) InsertModerationAction(_ context.Context, m ModerationAction) (ModerationAction, error) {
	switch {
	case m.ID == uuid.Nil() || m.ActorUserID == uuid.Nil():
		return ModerationAction{}, problem.ErrInternal
	case !m.TargetType.Valid(), !m.Action.postAction() && !m.Action.orgAction(), !reasonCodeRe.MatchString(m.ReasonCode),
		runes(m.Note) > 1000:
		return ModerationAction{}, errCheck
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ReportID != nil {
		if _, ok := s.reports[*m.ReportID]; !ok { // moderation_actions_report_id_fkey
			return ModerationAction{}, ErrNotFound
		}
	}
	if slices.ContainsFunc(s.actions, func(x ModerationAction) bool { return x.ID == m.ID }) {
		return ModerationAction{}, problem.ErrConflict
	}
	m.CreatedAt = s.stamp(time.Time{})
	s.actions = append(s.actions, m)
	return m, nil
}

// GetModerationAction implements Store.
func (s *MemStore) GetModerationAction(_ context.Context, id uuid.UUID) (ModerationAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.actions {
		if m.ID == id {
			return m, nil
		}
	}
	return ModerationAction{}, ErrNotFound
}

// ListModerationActions implements Store.
func (s *MemStore) ListModerationActions(_ context.Context, f ActionFilter, after *uuid.UUID, limit int) ([]ModerationAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ModerationAction{}
	for _, m := range s.actions {
		if f.TargetType != "" && m.TargetType != f.TargetType || f.TargetID != nil && m.TargetID != *f.TargetID ||
			after != nil && m.ID.Compare(*after) >= 0 {
			continue
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b ModerationAction) int { return -a.ID.Compare(b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LatestActions implements Store.
func (s *MemStore) LatestActions(_ context.Context, t TargetType, ids []uuid.UUID) (map[uuid.UUID]ModerationAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]ModerationAction{}
	for _, m := range s.actions {
		if m.TargetType != t || !slices.Contains(ids, m.TargetID) {
			continue
		}
		if cur, ok := out[m.TargetID]; !ok || cmp.Or(m.CreatedAt.Compare(cur.CreatedAt), m.ID.Compare(cur.ID)) > 0 {
			out[m.TargetID] = m
		}
	}
	return out, nil
}

// ReportActions implements Store.
func (s *MemStore) ReportActions(_ context.Context, reportID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.actions {
		if m.ReportID != nil && *m.ReportID == reportID {
			n++
		}
	}
	return n, nil
}

// ActionsBy implements Store.
func (s *MemStore) ActionsBy(_ context.Context, userID uuid.UUID) ([]ModerationAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ModerationAction{}
	for _, m := range s.actions {
		if m.ActorUserID == userID {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b ModerationAction) int { return a.ID.Compare(b.ID) })
	return out, nil
}

// ListReview implements Store.
func (s *MemStore) ListReview(_ context.Context, after *After, limit int) ([]Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Post{}
	for _, p := range s.posts {
		if p.Status != StatusPendingReview {
			continue
		}
		if after != nil && cmp.Or(p.CreatedAt.Compare(after.At), p.ID.Compare(after.ID)) <= 0 {
			continue
		}
		out = append(out, p.clone())
	}
	slices.SortFunc(out, func(a, b Post) int { return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), a.ID.Compare(b.ID)) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// settingsRow returns the row of orgID, a new one when there is none.
func (s *MemStore) settingsRow(orgID uuid.UUID) OrgSettings {
	o, ok := s.settings[orgID]
	if !ok {
		t := s.stamp(time.Time{})
		o = OrgSettings{OrgID: orgID, TrustLevel: TrustNew, CreatedAt: t, UpdatedAt: t}
	}
	return o
}

// AddApprovedPost implements Store.
func (s *MemStore) AddApprovedPost(_ context.Context, orgID uuid.UUID) (OrgSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.settingsRow(orgID)
	o.ApprovedPostsCount++
	o.UpdatedAt = s.stamp(o.UpdatedAt)
	s.settings[orgID] = o
	return o, nil
}

// PromoteTrusted implements Store.
func (s *MemStore) PromoteTrusted(_ context.Context, orgID uuid.UUID, threshold int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.settings[orgID]
	if !ok || o.TrustLevel != TrustNew || o.ApprovedPostsCount < threshold {
		return false, nil
	}
	o.TrustLevel, o.UpdatedBy, o.UpdatedAt = TrustTrusted, nil, s.stamp(o.UpdatedAt)
	s.settings[orgID] = o
	return true, nil
}

// SetTrust implements Store.
func (s *MemStore) SetTrust(_ context.Context, in OrgSettings) (OrgSettings, error) {
	switch in.TrustLevel {
	case TrustNew, TrustTrusted, TrustRestricted, TrustSuspended:
	default:
		return OrgSettings{}, errCheck
	}
	if runes(in.Reason) > 500 || (in.TrustLevel == TrustRestricted || in.TrustLevel == TrustSuspended) && in.Reason == nil {
		return OrgSettings{}, errCheck
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.settingsRow(in.OrgID)
	o.TrustLevel, o.RestrictedUntil, o.Reason, o.UpdatedBy = in.TrustLevel, in.RestrictedUntil, in.Reason, in.UpdatedBy
	o.UpdatedAt = s.stamp(o.UpdatedAt)
	s.settings[in.OrgID] = o
	return o, nil
}
