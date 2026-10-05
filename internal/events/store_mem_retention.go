package events

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"
)

// PurgeExpired implements Store (same contract as the GORM store).
func (s *MemStore) PurgeExpired(_ context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type due struct {
		end time.Time
		key [2]uuid.UUID
	}
	ended := func(eventID uuid.UUID) (time.Time, bool) {
		e, ok := s.events[eventID]
		return e.EndsAt, ok && e.EndsAt.Before(cutoff)
	}
	var rows []due
	switch policy {
	case PolicyFormAnswers, PolicyFormAnswersSensitive:
		for id, r := range s.regs {
			end, ok := ended(r.EventID)
			pending := r.AnswersPurgedAt == nil
			if policy == PolicyFormAnswersSensitive {
				pending = r.AnswersSensitiveEnc != nil
			}
			if ok && pending {
				rows = append(rows, due{end, [2]uuid.UUID{id}})
			}
		}
	case PolicyStaff:
		for _, st := range s.staff {
			if end, ok := ended(st.EventID); ok {
				rows = append(rows, due{end, [2]uuid.UUID{st.EventID, st.UserID}})
			}
		}
	default:
		return 0, fmt.Errorf("events: no retention purge for policy %q", policy)
	}
	if dryRun {
		return len(rows), nil
	}
	slices.SortFunc(rows, func(a, b due) int {
		return cmp.Or(a.end.Compare(b.end), bytes.Compare(a.key[0][:], b.key[0][:]), bytes.Compare(a.key[1][:], b.key[1][:]))
	})
	rows = rows[:min(len(rows), max(limit, 0))]
	now := s.now().UTC().Truncate(time.Microsecond)
	for _, d := range rows {
		switch policy {
		case PolicyFormAnswers:
			r := s.regs[d.key[0]]
			r.Answers, r.AnswersSensitiveEnc, r.AnswersPurgedAt = JSON(`{}`), nil, &now
			s.regs[d.key[0]] = r
		case PolicyFormAnswersSensitive:
			r := s.regs[d.key[0]]
			r.AnswersSensitiveEnc = nil
			s.regs[d.key[0]] = r
		case PolicyStaff:
			s.staff = slices.DeleteFunc(s.staff, func(st Staff) bool { return st.EventID == d.key[0] && st.UserID == d.key[1] })
		}
	}
	return len(rows), nil
}
