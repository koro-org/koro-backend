package identity

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"
)

// retained is a row whose retention ended: its anchor and ID.
type retained struct {
	at time.Time
	id uuid.UUID
}

func oldestFirst(rows []retained) {
	slices.SortFunc(rows, func(a, b retained) int { return cmp.Or(a.at.Compare(b.at), bytes.Compare(a.id[:], b.id[:])) })
}

// PurgeExpired implements Store (the approval check of 000121's
// privileged_account_actions_purge is the kvkk dry-run rule here).
func (m *MemStore) PurgeExpired(_ context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rows []retained
	switch policy {
	case PolicyRoleHistory:
		for id, a := range m.roles {
			end := a.RevokedAt
			if end == nil {
				end = a.RejectedAt
			}
			if (a.Status == RoleRevoked || a.Status == RoleRejected) && end != nil && end.Before(cutoff) {
				rows = append(rows, retained{*end, id})
			}
		}
	case PolicyPrivilegedActionHistory:
		for id, r := range m.actions {
			end := r.ExpiresAt
			if r.Status == AccountActionApproved && r.ApprovedAt != nil {
				end = *r.ApprovedAt
			}
			if end.Before(cutoff) {
				rows = append(rows, retained{r.RequestedAt, id})
			}
		}
	default:
		return 0, fmt.Errorf("identity: no retention purge for policy %q", policy)
	}
	if dryRun {
		return len(rows), nil
	}
	oldestFirst(rows)
	rows = rows[:min(len(rows), max(limit, 0))]
	for _, r := range rows {
		delete(m.roles, r.id)
		delete(m.actions, r.id)
	}
	return len(rows), nil
}

// eduDue returns the rows the policy is due for at cutoff with their
// anchors (see the GORM store's eduDueSQL).
func (m *MemStore) eduDue(policy string, cutoff time.Time) ([]retained, error) {
	var rows []retained
	for id, v := range m.edu {
		var at time.Time
		switch policy {
		case PolicyEduDocument:
			if v.Method != EduMethodDocument || v.DocumentFileID == nil || v.Status == EduPending {
				continue
			}
			at = v.UpdatedAt
			if v.VerifiedAt != nil {
				at = *v.VerifiedAt
			}
		case PolicyEduVerificationHistory:
			switch v.Status {
			case EduRejected, EduRevoked, EduSuperseded:
				at = v.UpdatedAt
			case EduExpired:
				at = v.UpdatedAt
				if v.ExpiresAt != nil {
					at = *v.ExpiresAt
				}
			default:
				continue
			}
		default:
			return nil, fmt.Errorf("identity: no retention purge for policy %q", policy)
		}
		if at.Before(cutoff) {
			rows = append(rows, retained{at, id})
		}
	}
	return rows, nil
}

// EduRetentionDue implements Store.
func (m *MemStore) EduRetentionDue(_ context.Context, policy string, cutoff time.Time, limit int) ([]EduRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, err := m.eduDue(policy, cutoff)
	if err != nil {
		return nil, err
	}
	oldestFirst(rows)
	rows = rows[:min(len(rows), max(limit, 0))]
	out := make([]EduRecord, len(rows))
	for i, r := range rows {
		out[i] = m.edu[r.id].clone()
	}
	return out, nil
}

// CountEduRetentionDue implements Store.
func (m *MemStore) CountEduRetentionDue(_ context.Context, policy string, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, err := m.eduDue(policy, cutoff)
	return len(rows), err
}
