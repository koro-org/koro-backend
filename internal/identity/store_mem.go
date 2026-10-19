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
	"koro/internal/platform/textx"
)

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It mirrors the constraints the services rely on (unique
// address and token hashes, the open-assignment index and the main
// platform_role_assignments CHECKs) and passes the same contract test as
// the GORM store. Transactions are not emulated (db.NopTx). The store
// stamps created_at and updated_at of users, edu_verifications and
// mfa_totp with its own clock, as the column defaults and the touch
// triggers do for gormStore (one clock, BE-069): a caller's values are
// ignored.
type MemStore struct {
	mu       sync.Mutex
	now      func() time.Time
	users    map[uuid.UUID]User
	sessions map[uuid.UUID]Session
	tokens   map[uuid.UUID]EmailToken
	roles    map[uuid.UUID]RoleAssignment
	totp     map[uuid.UUID]TOTP
	recovery map[uuid.UUID]memRecovery
	actions  map[uuid.UUID]AccountActionRequest
	edu      map[uuid.UUID]EduRecord
}

type memRecovery struct {
	userID uuid.UUID
	hash   []byte
	usedAt *time.Time
}

// NewMemStore returns an empty in-memory store; now stamps created_at and
// updated_at.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{
		now:   now,
		users: map[uuid.UUID]User{}, sessions: map[uuid.UUID]Session{},
		tokens: map[uuid.UUID]EmailToken{}, roles: map[uuid.UUID]RoleAssignment{},
		totp: map[uuid.UUID]TOTP{}, recovery: map[uuid.UUID]memRecovery{}, actions: map[uuid.UUID]AccountActionRequest{},
		edu: map[uuid.UUID]EduRecord{},
	}
}

var _ Store = (*MemStore)(nil)

// SeedAt runs seed with the store's clock pinned at t, so the rows it
// writes are stamped at t (fixtures of a known age). The timestamps stay
// the store's own, as the database's are for gormStore: callers never set
// them. seed must not run alongside other writers of the store.
func (m *MemStore) SeedAt(t time.Time, seed func() error) error {
	m.mu.Lock()
	prev := m.now
	m.now = func() time.Time { return t }
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.now = prev
		m.mu.Unlock()
	}()
	return seed()
}

// stamp is the store's clock; mu is held.
func (m *MemStore) stamp() time.Time { return m.now().UTC().Truncate(time.Microsecond) }

// touch is the updated_at of a row changed now: never before its last
// stamp (a test clock set back); mu is held.
func (m *MemStore) touch(prev time.Time) time.Time {
	if t := m.stamp(); t.After(prev) {
		return t
	}
	return prev
}

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
	case u.Status == StatusSuspended && u.SuspendedAt == nil:
		return fmt.Errorf("%w: users_suspended_chk", errMemCheck)
	case len([]rune(u.SuspensionReason)) > 500:
		return fmt.Errorf("%w: users_suspension_reason_chk", errMemCheck)
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
	now := m.stamp()
	u.CreatedAt, u.UpdatedAt = now, now
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
	cur.MFAEnabled, cur.SuspendedAt, cur.SuspensionReason = u.MFAEnabled, cloneTime(u.SuspendedAt), u.SuspensionReason
	if err := checkUser(cur); err != nil {
		return err
	}
	cur.UpdatedAt = m.touch(cur.UpdatedAt)
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
		ErasedAt: &at, CreatedAt: u.CreatedAt, UpdatedAt: m.touch(u.UpdatedAt),
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
	case (a.RevokeRequestedBy == nil) != (a.RevokeRequestedAt == nil) ||
		(a.Status == RoleRevokePending && a.RevokeRequestedBy == nil) ||
		(a.RevokeRequestedBy != nil && a.Status != RoleRevokePending && a.Status != RoleRevoked):
		return bad("revoke_request")
	case privilegedRole(a.Role) && a.Status == RoleRevoked && (a.RevokeRequestedBy == nil || same(a.RevokedBy, a.RevokeRequestedBy)):
		return bad("revoke_four_eyes")
	}
	return nil
}

// memAssignmentErr maps a CHECK failure to the error the GORM store's
// constraint table gives (store contract).
func memAssignmentErr(err error) error {
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "_four_eyes_chk"):
		return ErrFourEyes.Wrap(err)
	case strings.Contains(err.Error(), "_self_chk"):
		return ErrForbidden.Wrap(err)
	}
	return err
}

// InsertRoleAssignment implements Store.
func (m *MemStore) InsertRoleAssignment(_ context.Context, a RoleAssignment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[a.UserID]; !ok {
		return fmt.Errorf("identity memstore: assignment of unknown user: %w", ErrNotFound)
	}
	if err := memAssignmentErr(checkAssignment(a)); err != nil {
		return err
	}
	if _, dup := m.roles[a.ID]; dup {
		return fmt.Errorf("identity memstore: duplicate assignment %s", a.ID)
	}
	if err := m.openConflict(a); err != nil {
		return err
	}
	m.roles[a.ID] = a.clone()
	return nil
}

func openStatus(s RoleStatus) bool { return s == RolePending || s.InEffect() }

// openConflict mirrors platform_role_assignments_open_uq.
func (m *MemStore) openConflict(a RoleAssignment) error {
	if !openStatus(a.Status) {
		return nil
	}
	for _, o := range m.roles {
		if o.ID != a.ID && o.UserID == a.UserID && o.Role == a.Role && openStatus(o.Status) {
			return ErrRoleAssignmentOpen.Wrap(errors.New("platform_role_assignments_open_uq"))
		}
	}
	return nil
}

// RoleAssignment implements Store.
func (m *MemStore) RoleAssignment(_ context.Context, id uuid.UUID) (RoleAssignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.roles[id]
	if !ok {
		return RoleAssignment{}, ErrNotFound
	}
	return a.clone(), nil
}

// LockRoleAssignment implements Store.
func (m *MemStore) LockRoleAssignment(ctx context.Context, id uuid.UUID) (RoleAssignment, error) {
	return m.RoleAssignment(ctx, id)
}

// UpdateRoleAssignment implements Store.
func (m *MemStore) UpdateRoleAssignment(_ context.Context, a RoleAssignment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.roles[a.ID]
	if !ok {
		return ErrNotFound
	}
	cur.Status, cur.ApprovedBy, cur.ApprovedAt, cur.GrantedAt = a.Status, cloneID(a.ApprovedBy), cloneTime(a.ApprovedAt), cloneTime(a.GrantedAt)
	cur.RejectedBy, cur.RejectedAt, cur.RevokeReason = cloneID(a.RejectedBy), cloneTime(a.RejectedAt), a.RevokeReason
	cur.RevokeRequestedBy, cur.RevokeRequestedAt = cloneID(a.RevokeRequestedBy), cloneTime(a.RevokeRequestedAt)
	cur.RevokedBy, cur.RevokedAt = cloneID(a.RevokedBy), cloneTime(a.RevokedAt)
	if err := memAssignmentErr(checkAssignment(cur)); err != nil {
		return err
	}
	if err := m.openConflict(cur); err != nil {
		return err
	}
	m.roles[a.ID] = cur
	return nil
}

// RoleAssignments implements Store.
func (m *MemStore) RoleAssignments(_ context.Context, q RoleQuery) ([]RoleAssignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RoleAssignment
	for _, a := range m.roles {
		if (q.Role == "" || a.Role == q.Role) && (q.IncludeEnded || openStatus(a.Status)) &&
			(q.After == nil || a.ID.Compare(*q.After) < 0) {
			out = append(out, a.clone())
		}
	}
	slices.SortFunc(out, func(a, b RoleAssignment) int { return b.ID.Compare(a.ID) })
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// CountRoleAssignments implements Store.
func (m *MemStore) CountRoleAssignments(_ context.Context, role authz.PlatformRole) (int, int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var eff, pend, rp int
	for _, a := range m.roles {
		if a.Role != role {
			continue
		}
		if a.Status.InEffect() {
			eff++
		}
		switch a.Status {
		case RolePending:
			pend++
		case RoleRevokePending:
			rp++
		default:
		}
	}
	return eff, pend, rp, nil
}

// SearchUsers implements Store. The name match folds case the Turkish way
// (koro_norm without unaccent: tests use plain names).
func (m *MemStore) SearchUsers(_ context.Context, q UserSearch) ([]User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []User
	name := textx.LowerTR(q.Name)
	for _, u := range m.users {
		switch {
		case q.Email != "" && u.EmailNormalized != q.Email,
			q.Email == "" && q.Name != "" && (u.Status == StatusErased || !strings.Contains(textx.LowerTR(u.FirstName+" "+u.LastName), name)),
			q.Status != "" && u.Status != q.Status,
			q.After != nil && u.ID.Compare(*q.After) >= 0:
			continue
		}
		out = append(out, u.clone())
	}
	slices.SortFunc(out, func(a, b User) int { return b.ID.Compare(a.ID) })
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// MarkSessionMFA implements Store.
func (m *MemStore) MarkSessionMFA(_ context.Context, userID, id uuid.UUID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.UserID != userID || s.RevokedAt != nil {
		return false, nil
	}
	s.MFAVerifiedAt = &at
	m.sessions[id] = s
	return true, nil
}

// TOTP implements Store.
func (m *MemStore) TOTP(_ context.Context, userID uuid.UUID) (TOTP, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.totp[userID]
	if !ok {
		return TOTP{}, false, nil
	}
	return t.clone(), true, nil
}

// PutTOTP implements Store.
func (m *MemStore) PutTOTP(_ context.Context, t TOTP) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[t.UserID]; !ok {
		return fmt.Errorf("identity memstore: totp of unknown user: %w", ErrNotFound)
	}
	if cur, ok := m.totp[t.UserID]; ok && cur.ConfirmedAt != nil {
		return ErrMFAAlreadyEnabled
	}
	n := t.clone()
	n.ConfirmedAt, n.LastUsedStep, n.CreatedAt = nil, nil, m.stamp() // a new enrollment: created now
	m.totp[t.UserID] = n
	return nil
}

// ConfirmTOTP implements Store.
func (m *MemStore) ConfirmTOTP(_ context.Context, userID uuid.UUID, step int64, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.totp[userID]
	if !ok || t.ConfirmedAt != nil {
		return false, nil
	}
	t.ConfirmedAt, t.LastUsedStep = &at, &step
	m.totp[userID] = t
	return true, nil
}

// UseTOTPStep implements Store.
func (m *MemStore) UseTOTPStep(_ context.Context, userID uuid.UUID, step int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.totp[userID]
	if !ok || t.ConfirmedAt == nil || (t.LastUsedStep != nil && *t.LastUsedStep >= step) {
		return false, nil
	}
	t.LastUsedStep = &step
	m.totp[userID] = t
	return true, nil
}

// DeleteTOTP implements Store.
func (m *MemStore) DeleteTOTP(_ context.Context, userID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.totp[userID]; !ok {
		return 0, nil
	}
	delete(m.totp, userID)
	return 1, nil
}

// ReplaceRecoveryCodes implements Store.
func (m *MemStore) ReplaceRecoveryCodes(_ context.Context, userID uuid.UUID, hashes [][]byte, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.recovery {
		if r.userID == userID {
			delete(m.recovery, id)
		}
	}
	for _, h := range hashes {
		if len(h) != 32 {
			return fmt.Errorf("%w: mfa_recovery_codes_code_hash_chk", errMemCheck)
		}
		for _, r := range m.recovery {
			if bytes.Equal(r.hash, h) {
				return errors.New("identity memstore: mfa_recovery_codes_code_hash_uq")
			}
		}
		m.recovery[uuid.NewV7()] = memRecovery{userID: userID, hash: bytes.Clone(h)}
	}
	return nil
}

// UseRecoveryCode implements Store.
func (m *MemStore) UseRecoveryCode(_ context.Context, userID uuid.UUID, hash []byte, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.recovery {
		if r.userID == userID && r.usedAt == nil && bytes.Equal(r.hash, hash) {
			r.usedAt = &at
			m.recovery[id] = r
			return true, nil
		}
	}
	return false, nil
}

// RecoveryCodesLeft implements Store.
func (m *MemStore) RecoveryCodesLeft(_ context.Context, userID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.recovery {
		if r.userID == userID && r.usedAt == nil {
			n++
		}
	}
	return n, nil
}

// DeleteRecoveryCodes implements Store.
func (m *MemStore) DeleteRecoveryCodes(_ context.Context, userID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.recovery {
		if r.userID == userID {
			delete(m.recovery, id)
			n++
		}
	}
	return n, nil
}

// checkAccountAction mirrors the privileged_account_actions CHECKs.
func checkAccountAction(r AccountActionRequest) error {
	bad := func(name string) error { return fmt.Errorf("%w: privileged_account_actions_%s_chk", errMemCheck, name) }
	switch {
	case r.Action != AccountSuspend && r.Action != AccountRevokeSessions && r.Action != AccountResetMFA:
		return bad("action")
	case len([]rune(r.Reason)) < 5 || len([]rune(r.Reason)) > 500:
		return bad("reason")
	case r.RequestedBy == r.TargetUserID || (r.ApprovedBy != nil && *r.ApprovedBy == r.TargetUserID):
		return ErrForbidden.Wrap(bad("self"))
	case r.ApprovedBy != nil && *r.ApprovedBy == r.RequestedBy:
		return ErrFourEyes.Wrap(bad("four_eyes"))
	case r.Status == AccountActionApproved && (r.ApprovedBy == nil || r.ApprovedAt == nil):
		return bad("approved")
	case r.Status != AccountActionApproved && (r.ApprovedBy != nil || r.ApprovedAt != nil):
		return bad("unapproved")
	case !r.ExpiresAt.After(r.RequestedAt):
		return bad("expiry")
	}
	return nil
}

func (m *MemStore) actionOpen(r AccountActionRequest) error {
	if r.Status != AccountActionPending {
		return nil
	}
	for _, o := range m.actions {
		if o.ID != r.ID && o.TargetUserID == r.TargetUserID && o.Action == r.Action && o.Status == AccountActionPending {
			return ErrAccountActionOpen.Wrap(errors.New("privileged_account_actions_open_uq"))
		}
	}
	return nil
}

// InsertAccountAction implements Store.
func (m *MemStore) InsertAccountAction(_ context.Context, r AccountActionRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[r.TargetUserID]; !ok {
		return fmt.Errorf("identity memstore: action against unknown user: %w", ErrNotFound)
	}
	if _, dup := m.actions[r.ID]; dup {
		return fmt.Errorf("identity memstore: duplicate account action %s", r.ID)
	}
	if err := checkAccountAction(r); err != nil {
		return err
	}
	if err := m.actionOpen(r); err != nil {
		return err
	}
	m.actions[r.ID] = r.clone()
	return nil
}

// LockOpenAccountAction implements Store.
func (m *MemStore) LockOpenAccountAction(_ context.Context, target uuid.UUID, action AccountActionKind) (AccountActionRequest, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.actions {
		if r.TargetUserID == target && r.Action == action && r.Status == AccountActionPending {
			return r.clone(), true, nil
		}
	}
	return AccountActionRequest{}, false, nil
}

// UpdateAccountAction implements Store.
func (m *MemStore) UpdateAccountAction(_ context.Context, r AccountActionRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.actions[r.ID]
	if !ok {
		return ErrNotFound
	}
	cur.Status, cur.ApprovedBy, cur.ApprovedAt = r.Status, cloneID(r.ApprovedBy), cloneTime(r.ApprovedAt)
	if err := checkAccountAction(cur); err != nil {
		return err
	}
	m.actions[r.ID] = cur
	return nil
}

// AccountActionsOf implements Store.
func (m *MemStore) AccountActionsOf(_ context.Context, userID uuid.UUID) ([]AccountActionRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AccountActionRequest
	for _, r := range m.actions {
		if r.TargetUserID == userID {
			out = append(out, r.clone())
		}
	}
	slices.SortFunc(out, func(a, b AccountActionRequest) int { return a.ID.Compare(b.ID) })
	return out, nil
}
