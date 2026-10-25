package identity

import (
	"context"
	"slices"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/textx"
)

// Me is getMe (04 §V.1, identity part): the account, the actor's roles and
// the menu hints. Student profile sections belong to students (BE-022).
type Me struct {
	UserID          uuid.UUID
	Email           string
	FirstName       string
	LastName        string
	Locale          string
	Persona         Persona
	MFAEnabled      bool
	EmailVerifiedAt *time.Time
	BirthDate       *civil.Date
	IsMinor         bool
	StudentStatus   authz.StudentStatus
	UniversityID    *uuid.UUID
	Memberships     []authz.Membership // sensitive clubs included: only the owner sees this
	PlatformRoles   []authz.PlatformRole
	// Capabilities are menu hints (04 §2.1): actions the client may offer.
	// They are not authorization decisions; every request is checked again.
	Capabilities []authz.Action
	// Terms is the caller's position towards the terms of use (BE-118).
	Terms TermsState
}

// MePatch is the body of updateMe; nil fields stay.
type MePatch struct {
	FirstName *string
	LastName  *string
	Locale    *string
}

// Me implements Service (getMe).
func (s *service) Me(ctx context.Context, a authz.Actor) (Me, error) {
	if err := authz.Can(ctx, a, authz.IdentityAccountReadOwn, ownUser(a)); err != nil {
		return Me{}, err
	}
	u, err := s.d.Store.User(ctx, a.UserID)
	if err != nil {
		return Me{}, err
	}
	return s.me(ctx, u, a)
}

func (s *service) me(ctx context.Context, u User, a authz.Actor) (Me, error) {
	terms, err := s.termsState(ctx, u)
	if err != nil {
		return Me{}, err
	}
	return Me{
		UserID: u.ID, Email: u.Email, FirstName: u.FirstName, LastName: u.LastName, Locale: u.Locale,
		Persona: u.Persona, MFAEnabled: u.MFAEnabled, EmailVerifiedAt: u.EmailVerifiedAt, BirthDate: u.clone().BirthDate,
		IsMinor: u.IsMinor(s.now()), StudentStatus: a.StudentStatus, UniversityID: a.UniversityID,
		Memberships: slices.Clone(a.Memberships), PlatformRoles: slices.Clone(a.PlatformRoles),
		Capabilities: Capabilities(a), Terms: terms,
	}, nil
}

// UpdateMe implements Service (updateMe): names and locale.
func (s *service) UpdateMe(ctx context.Context, a authz.Actor, p MePatch) (Me, error) {
	if err := authz.Can(ctx, a, authz.IdentityAccountUpdateOwn, ownUser(a)); err != nil {
		return Me{}, err
	}
	if p.FirstName == nil && p.LastName == nil && p.Locale == nil {
		return Me{}, ErrEmptyPatch
	}
	var first, last string
	var err error
	if p.FirstName != nil {
		if first, err = cleanName(*p.FirstName, "body.first_name"); err != nil {
			return Me{}, err
		}
	}
	if p.LastName != nil {
		if last, err = cleanName(*p.LastName, "body.last_name"); err != nil {
			return Me{}, err
		}
	}
	if p.Locale != nil {
		if err := checkLocale(*p.Locale, "body.locale"); err != nil {
			return Me{}, err
		}
	}
	var out User
	var localeChanged bool
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.d.Store.LockUser(ctx, a.UserID)
		if err != nil {
			return err
		}
		var fields []string
		if p.FirstName != nil && first != u.FirstName {
			u.FirstName, fields = first, append(fields, "first_name")
		}
		if p.LastName != nil && last != u.LastName {
			u.LastName, fields = last, append(fields, "last_name")
		}
		if p.Locale != nil && *p.Locale != u.Locale {
			u.Locale, fields, localeChanged = *p.Locale, append(fields, "locale"), true
		}
		out = u
		if len(fields) == 0 {
			return nil
		}
		if err := s.d.Store.UpdateUser(ctx, u); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, event(authz.IdentityAccountUpdateOwn, audit.Success, resourceUser, u.ID,
			map[string]any{"fields": fields}).By(a))
	})
	if err != nil {
		return Me{}, err
	}
	if localeChanged { // the terms in force may be another language's version (BE-124)
		s.gate.forget(a.UserID)
	}
	return s.me(ctx, out, a)
}

// ---- Users (01 §4.6)

func (s *service) summary(u User) UserSummary {
	return UserSummary{ID: u.ID, Status: u.Status, FirstName: u.FirstName, LastName: u.LastName, Locale: u.Locale, IsMinor: u.IsMinor(s.now())}
}

// Get implements Users.
func (s *service) Get(ctx context.Context, id uuid.UUID) (UserSummary, error) {
	u, err := s.d.Store.User(ctx, id)
	if err != nil {
		return UserSummary{}, err
	}
	return s.summary(u), nil
}

// GetMany implements Users.
func (s *service) GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]UserSummary, error) {
	out := make(map[uuid.UUID]UserSummary, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	us, err := s.d.Store.Users(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, u := range us {
		out[u.ID] = s.summary(u)
	}
	return out, nil
}

// Contact implements Users (worker/notification path only).
func (s *service) Contact(ctx context.Context, id uuid.UUID) (UserContact, error) {
	u, err := s.d.Store.User(ctx, id)
	if err != nil {
		return UserContact{}, err
	}
	if u.Status == StatusErased || u.Email == "" {
		return UserContact{}, ErrNotFound
	}
	return UserContact{ID: u.ID, Email: u.Email, FirstName: u.FirstName, LastName: u.LastName, Locale: u.Locale}, nil
}

// FindByEmail implements Users.
func (s *service) FindByEmail(ctx context.Context, email string) (uuid.UUID, bool, error) {
	n := textx.NormalizeEmail(email)
	if n == "" {
		return uuid.Nil(), false, nil
	}
	u, found, err := s.d.Store.UserByEmail(ctx, n)
	if err != nil || !found {
		return uuid.Nil(), false, err
	}
	return u.ID, true, nil
}
