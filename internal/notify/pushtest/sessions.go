package pushtest

import (
	"context"
	"slices"
	"sync"
	"uuid"
)

// Sessions is a notify.UnrevokedSessions double: a set of open sessions, shared
// by every user (a session ID belongs to one user anyway).
type Sessions struct {
	mu    sync.Mutex
	open  []uuid.UUID
	err   error
	calls int
}

// NewSessions returns a double with the given sessions open.
func NewSessions(open ...uuid.UUID) *Sessions {
	return &Sessions{open: slices.Clone(open)}
}

// Open marks a session open.
func (s *Sessions) Open(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = append(s.open, id)
}

// Close ends a session.
func (s *Sessions) Close(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = slices.DeleteFunc(s.open, func(o uuid.UUID) bool { return o == id })
}

// Fail makes every lookup return err; nil heals it.
func (s *Sessions) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Calls is how many lookups were made.
func (s *Sessions) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// UnrevokedSessionIDs implements notify.UnrevokedSessions.
func (s *Sessions) UnrevokedSessionIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return slices.Clone(s.open), nil
}
