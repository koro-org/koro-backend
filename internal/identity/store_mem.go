package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/authz"
)

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It mirrors the constraints the services rely on (unique
// address and token hashes, the open-assignment index and the main
// platform_role_assignments CHECKs) and passes the same contract test as
// the GORM store. Transactions are not emulated (db.NopTx).
type MemStore struct {
	mu       sync.Mutex
	users    map[uuid.UUID]User
	sessions map[uuid.UUID]Session
	tokens   map[uuid.UUID]EmailToken
	roles    map[uuid.UUID]RoleAssignment
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{
		users: map[uuid.UUID]User{}, sessions: map[uuid.UUID]Session{},
		tokens: map[uuid.UUID]EmailToken{}, roles: map[uuid.UUID]RoleAssignment{},
	}
}

var _ Store = (*MemStore)(nil)

var errMemCheck = errors.New("identity memstore: check constraint")

func (m *MemStore) emailTaken(normalized string, except uuid.UUID) bool {
	if normalized == "" {
		return false
	}
	for _, u := range m.users {
		if u.ID != except && u.EmailNormalized == normalized {
			return true
		}
	}
	return false
}

func checkUser(u User) error {
	switch {
	case u.Status == StatusErased && (u.Email != "" || u.PasswordHash != "" || u.FirstName != "" || u.BirthDate != nil || u.ErasedAt == nil):
		return fmt.Errorf("%w: users_erased_chk", errMemCheck)
	case u.Status != StatusErased && (u.Email == "" || u.EmailNormalized == "" || u.FirstName == "" || u.LastName == ""):
		return fmt.Errorf("%w: users_live_chk", errMemCheck)
	case u.Persona == PersonaStudent && u.Status != StatusErased && u.BirthDate == nil:
		return fmt.Errorf("%w: users_student_birth_date_chk", errMemCheck)
	case u.FailedLogins < 0 || u.FailedLogins > maxFailedLogins:
		return fmt.Errorf("%w: users_failed_login_count_chk", errMemCheck)
	}
	return nil
}

// InsertUser implements Store.
func (m *MemStore) InsertUser(_ context.Context, u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.users[u.ID]; dup {
		return fmt.Errorf("identity memstore: duplicate user %s", u.ID)
	}
	if m.emailTaken(u.EmailNormalized, u.ID) {
		return ErrEmailTaken
	}
	if err := checkUser(u); err != nil {
		return err
	}
	m.users[u.ID] = u.clone()
	return nil
}

// User implements Store.
func (m *MemStore) User(_ context.Context, id uuid.UUID) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u.clone(), nil
}

// LockUser implements Store.
func (m *MemStore) LockUser(ctx context.Context, id uuid.UUID) (User, error) { return m.User(ctx, id) }

// UserByEmail implements Store.
func (m *MemStore) UserByEmail(_ context.Context, normalized string) (User, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if normalized == "" {
		return User{}, false, nil
	}
	for _, u := range m.users {
		if u.EmailNormalized == normalized {
			return u.clone(), true, nil
		}
	}
	return User{}, false, nil
}

// Users implements Store.
func (m *MemStore) Users(_ context.Context, ids []uuid.UUID) ([]User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []User
	for _, id := range ids {
		if u, ok := m.users[id]; ok && !slices.ContainsFunc(out, func(x User) bool { return x.ID == id }) {
			out = append(out, u.clone())
		}
	}
	return out, nil
}

// UpdateUser implements Store.
func (m *MemStore) UpdateUser(_ context.Context, u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.users[u.ID]
	if !ok {
		return ErrNotFound
	}
	if m.emailTaken(u.EmailNormalized, u.ID) {
		return ErrEmailTaken
	}
	cur.Status, cur.Email, cur.EmailNormalized, cur.EmailVerifiedAt = u.Status, u.Email, u.EmailNormalized, cloneTime(u.EmailVerifiedAt)
	cur.PasswordHash, cur.FirstName, cur.LastName, cur.Locale = u.PasswordHash, u.FirstName, u.LastName, u.Locale
	cur.LastLoginAt, cur.FailedLogins, cur.LockedUntil = cloneTime(u.LastLoginAt), u.FailedLogins, cloneTime(u.LockedUntil)
	if err := checkUser(cur); err != nil {
		return err
	}
	m.users[u.ID] = cur
	return nil
}

// EraseUser implements Store.
func (m *MemStore) EraseUser(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return false, ErrNotFound
	}
	if u.Status == StatusErased {
		return false, nil
	}
	m.users[id] = User{
		ID: u.ID, Status: StatusErased, Persona: u.Persona, Locale: u.Locale, MFAEnabled: false,
		ErasedAt: &at, CreatedAt: u.CreatedAt, UpdatedAt: at,
	}
	return true, nil
}

// InsertSession implements Store.
func (m *MemStore) InsertSession(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[s.UserID]; !ok {
		return fmt.Errorf("identity memstore: session of unknown user: %w", ErrNotFound)
	}
	if len(s.TokenHash) != 32 || s.IdleExpiresAt.After(s.AbsoluteExpiresAt) || !s.RevokeReason.valid() && s.RevokeReason != "" {
		return fmt.Errorf("%w: sessions", errMemCheck)
	}
	for _, o := range m.sessions {
		if bytes.Equal(o.TokenHash, s.TokenHash) || o.ID == s.ID {
			return errors.New("identity memstore: duplicate session")
		}
	}
	m.sessions[s.ID] = s.clone()
	return nil
}

// SessionByTokenHash implements Store.
func (m *MemStore) SessionByTokenHash(_ context.Context, hash []byte) (Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if bytes.Equal(s.TokenHash, hash) {
			return s.clone(), true, nil
		}
	}
	return Session{}, false, nil
}

// Session implements Store.
func (m *MemStore) Session(_ context.Context, id uuid.UUID) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return s.clone(), nil
}

// TouchSession implements Store.
func (m *MemStore) TouchSession(_ context.Context, id uuid.UUID, lastSeen, idle time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.RevokedAt != nil {
		return nil
	}
	if idle.After(s.AbsoluteExpiresAt) {
		idle = s.AbsoluteExpiresAt
	}
	s.LastSeenAt, s.IdleExpiresAt = lastSeen, idle
	m.sessions[id] = s
	return nil
}

// RevokeSession implements Store.
func (m *MemStore) RevokeSession(_ context.Context, userID, id uuid.UUID, at time.Time, reason RevokeReason) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.UserID != userID || s.RevokedAt != nil {
		return false, nil
	}
	s.RevokedAt, s.RevokeReason = &at, reason
	m.sessions[id] = s
	return true, nil
}

// RevokeUserSessions implements Store.
func (m *MemStore) RevokeUserSessions(_ context.Context, userID uuid.UUID, keep *uuid.UUID, at time.Time, reason RevokeReason) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, s := range m.sessions {
		if s.UserID != userID || s.RevokedAt != nil || (keep != nil && id == *keep) {
			continue
		}
		s.RevokedAt, s.RevokeReason = &at, reason
		m.sessions[id] = s
		n++
	}
	return n, nil
}

// ActiveSessions implements Store.
func (m *MemStore) ActiveSessions(_ context.Context, userID uuid.UUID, now time.Time, after *uuid.UUID, limit int) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if s.UserID == userID && s.live(now) && (after == nil || s.ID.Compare(*after) < 0) {
			out = append(out, s.clone())
		}
	}
	slices.SortFunc(out, func(a, b Session) int { return b.ID.Compare(a.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SessionsOf implements Store.
func (m *MemStore) SessionsOf(_ context.Context, userID uuid.UUID) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if s.UserID == userID {
			out = append(out, s.clone())
		}
	}
	slices.SortFunc(out, func(a, b Session) int { return a.ID.Compare(b.ID) })
	return out, nil
}

// sessionEnd is when a session stopped being usable.
func sessionEnd(s Session) time.Time {
	end := s.IdleExpiresAt
	if s.AbsoluteExpiresAt.Before(end) {
		end = s.AbsoluteExpiresAt
	}
	if s.RevokedAt != nil && s.RevokedAt.Before(end) {
		end = *s.RevokedAt
	}
	return end
}

// DeleteEndedSessions implements Store.
func (m *MemStore) DeleteEndedSessions(_ context.Context, cutoff time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, s := range m.sessions {
		if n < limit && sessionEnd(s).Before(cutoff) {
			delete(m.sessions, id)
			n++
		}
	}
	return n, nil
}

// DeleteUserSessions implements Store.
func (m *MemStore) DeleteUserSessions(_ context.Context, userID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, s := range m.sessions {
		if s.UserID == userID {
			delete(m.sessions, id)
			n++
		}
	}
	return n, nil
}

// InsertEmailToken implements Store.
func (m *MemStore) InsertEmailToken(_ context.Context, t EmailToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[t.UserID]; !ok {
		return fmt.Errorf("identity memstore: token of unknown user: %w", ErrNotFound)
	}
	if len(t.TokenHash) != 32 || ((t.Purpose == PurposeEmailChange || t.Purpose == PurposeEduEmail) && t.TargetEmail == "") {
		return fmt.Errorf("%w: email_tokens", errMemCheck)
	}
	for _, o := range m.tokens {
		if bytes.Equal(o.TokenHash, t.TokenHash) || o.ID == t.ID {
			return errors.New("identity memstore: duplicate token")
		}
	}
	m.tokens[t.ID] = t.clone()
	return nil
}

// CloseOpenTokens implements Store.
func (m *MemStore) CloseOpenTokens(_ context.Context, userID uuid.UUID, purpose TokenPurpose, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.tokens {
		if t.UserID == userID && t.Purpose == purpose && t.ConsumedAt == nil {
			t.ConsumedAt = &at
			m.tokens[id] = t
		}
	}
	return nil
}

// EmailToken implements Store.
func (m *MemStore) EmailToken(_ context.Context, id uuid.UUID) (EmailToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return EmailToken{}, ErrNotFound
	}
	return t.clone(), nil
}

// EmailTokenByHash implements Store.
func (m *MemStore) EmailTokenByHash(_ context.Context, hash []byte) (EmailToken, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tokens {
		if bytes.Equal(t.TokenHash, hash) {
			return t.clone(), true, nil
		}
	}
	return EmailToken{}, false, nil
}

// LockEmailToken implements Store.
func (m *MemStore) LockEmailToken(ctx context.Context, id uuid.UUID) (EmailToken, error) {
	return m.EmailToken(ctx, id)
}

// ConsumeEmailToken implements Store.
func (m *MemStore) ConsumeEmailToken(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok || t.ConsumedAt != nil {
		return false, nil
	}
	t.ConsumedAt = &at
	m.tokens[id] = t
	return true, nil
}

// DeleteEndedEmailTokens implements Store.
func (m *MemStore) DeleteEndedEmailTokens(_ context.Context, cutoff time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, t := range m.tokens {
		if n >= limit {
			break
		}
		if t.ExpiresAt.Before(cutoff) || (t.ConsumedAt != nil && t.ConsumedAt.Before(cutoff)) {
			delete(m.tokens, id)
			n++
		}
	}
	return n, nil
}

// DeleteUserEmailTokens implements Store.
func (m *MemStore) DeleteUserEmailTokens(_ context.Context, userID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, t := range m.tokens {
		if t.UserID == userID {
			delete(m.tokens, id)
			n++
		}
	}
	return n, nil
}

// PlatformRoles implements Store (06 H-33: in effect only).
func (m *MemStore) PlatformRoles(_ context.Context, userID uuid.UUID) ([]authz.PlatformRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []authz.PlatformRole
	for _, a := range m.roles {
		if a.UserID == userID && a.Status.InEffect() && !slices.Contains(out, a.Role) {
			out = append(out, a.Role)
		}
	}
	slices.Sort(out)
	return out, nil
}

// RoleAssignmentsOf implements Store.
func (m *MemStore) RoleAssignmentsOf(_ context.Context, userID uuid.UUID) ([]RoleAssignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RoleAssignment
	for _, a := range m.roles {
		if a.UserID == userID {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b RoleAssignment) int { return a.ID.Compare(b.ID) })
	return out, nil
}

// UsersWithRole implements Store.
func (m *MemStore) UsersWithRole(_ context.Context, role authz.PlatformRole) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []uuid.UUID
	for _, a := range m.roles {
		if a.Role == role && a.Status.InEffect() && !slices.Contains(out, a.UserID) {
			out = append(out, a.UserID)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return a.Compare(b) })
	return out, nil
}

// privilegedRole is the four-eyes set of 06 H-17.
func privilegedRole(r authz.PlatformRole) bool {
	return r == authz.PlatformAdmin || r == authz.PlatformFinanceAdmin || r == authz.PlatformTalentAuditor
}

// checkAssignment mirrors the platform_role_assignments CHECKs (02 §5.7).
func checkAssignment(a RoleAssignment) error {
	eq := func(x *uuid.UUID, y uuid.UUID) bool { return x != nil && *x == y }
	same := func(x, y *uuid.UUID) bool { return x != nil && y != nil && *x == *y }
	bad := func(name string) error { return fmt.Errorf("%w: platform_role_assignments_%s_chk", errMemCheck, name) }
	open := a.Status == RoleActive || a.Status == RoleRevokePending || a.Status == RoleRevoked
	switch {
	case len(strings.TrimSpace(a.Reason)) < 5 || len(a.Reason) > 500:
		return bad("reason")
	case eq(a.RequestedBy, a.UserID) || eq(a.ApprovedBy, a.UserID) || eq(a.RevokeRequestedBy, a.UserID) || eq(a.RevokedBy, a.UserID):
		return bad("self")
	case a.RequestedBy == nil && a.Reason != bootstrapReason:
		return bad("bootstrap")
	case same(a.ApprovedBy, a.RequestedBy):
		return bad("four_eyes")
	case privilegedRole(a.Role) && a.Status != RolePending && a.Status != RoleRejected && a.ApprovedBy == nil && a.RequestedBy != nil:
		return bad("privileged")
	case (a.ApprovedBy == nil) != (a.ApprovedAt == nil) || (a.ApprovedBy != nil && !open):
		return bad("approval")
	case open != (a.GrantedAt != nil):
		return bad("granted")
	case (a.RejectedBy == nil) != (a.RejectedAt == nil) || (a.Status == RoleRejected) != (a.RejectedAt != nil):
		return bad("rejected")
	case (a.RevokedBy == nil) != (a.RevokedAt == nil) || (a.Status == RoleRevoked) != (a.RevokedAt != nil) ||
		(a.Status == RoleRevokePending || a.Status == RoleRevoked) != (a.RevokeReason != ""):
		return bad("revoked")
	}
	return nil
}

// InsertRoleAssignment implements Store.
func (m *MemStore) InsertRoleAssignment(_ context.Context, a RoleAssignment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[a.UserID]; !ok {
		return fmt.Errorf("identity memstore: assignment of unknown user: %w", ErrNotFound)
	}
	if err := checkAssignment(a); err != nil {
		return err
	}
	isOpen := func(s RoleStatus) bool { return s == RolePending || s.InEffect() }
	if isOpen(a.Status) {
		for _, o := range m.roles {
			if o.UserID == a.UserID && o.Role == a.Role && isOpen(o.Status) {
				return ErrBootstrap.Wrap(errors.New("platform_role_assignments_open_uq"))
			}
		}
	}
	m.roles[a.ID] = a
	return nil
}
