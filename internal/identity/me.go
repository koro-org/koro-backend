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
	// IsMinor: the minor protections apply (MinorProtected: under 18, or a
	// student whose birth date is unknown).
	IsMinor bool
	// BirthDateRequired: a student (verified or lapsed) without a birth
	// date. The client asks for it (updateMe) before anything else; until
	// then the account is protected as a minor (Q-52, BE-202).
	BirthDateRequired bool
	StudentStatus     authz.StudentStatus
	UniversityID      *uuid.UUID
	Memberships       []authz.Membership // sensitive clubs included: only the owner sees this
	PlatformRoles     []authz.PlatformRole
	// Capabilities are menu hints (04 §2.1): actions the client may offer.
	// They are not authorization decisions; every request is checked again.
	Capabilities []authz.Action
	// Terms is the caller's position towards the terms of use (BE-118).
	Terms TermsState
	// Phone is the caller's verified number, masked (BE-172); nil: none.
	Phone *PhoneView
	// SMSAvailable: an SMS adapter is configured (the phone and SMS
	// operations answer 503 otherwise).
	SMSAvailable bool
}

// MePatch is the body of updateMe; nil fields stay.
type MePatch struct {
	FirstName *string
	LastName  *string
	Locale    *string
	// BirthDate is set once: accepted while the account has none (a
	// student verified before the age gate covered every path, BE-202);
	// the same date again changes nothing, another is 409
	// identity.birth_date_locked.
	BirthDate *civil.Date
}

// Me implements Service (getMe).
func (s *service) Me(ctx context.Context, a authz.Actor) (Me, error) {
	if err := authz.Can(ctx, a, authz.IdentityAccountReadOwn, ownUser(a)); err != nil {
		return Me{}, err
	}
	u, err := s.users().User(ctx, a.UserID)
	if err != nil {
		return Me{}, err
	}
	return s.me(ctx, u, a)
}

// me is the answer of getMe and updateMe for u. The age facts come from u
// (updateMe may just have set the birth date) and the actor's student
// status; the menu hints follow them.
func (s *service) me(ctx context.Context, u User, a authz.Actor) (Me, error) {
	student := WasStudent(a.StudentStatus)
	a.IsMinor = MinorProtected(u.BirthDate, student, s.now())
	terms, err := s.termsState(ctx, u)
	if err != nil {
		return Me{}, err
	}
	phone, err := s.myPhone(ctx, a, u.ID)
	if err != nil {
		return Me{}, err
	}
	return Me{
		UserID: u.ID, Email: u.Email, FirstName: u.FirstName, LastName: u.LastName, Locale: u.Locale,
		Persona: u.Persona, MFAEnabled: u.MFAEnabled, EmailVerifiedAt: u.EmailVerifiedAt, BirthDate: u.clone().BirthDate,
		IsMinor: a.IsMinor, BirthDateRequired: student && u.BirthDate == nil, StudentStatus: a.StudentStatus, UniversityID: a.UniversityID,
		Memberships: slices.Clone(a.Memberships), PlatformRoles: slices.Clone(a.PlatformRoles),
		Capabilities: Capabilities(a), Terms: terms, Phone: phone, SMSAvailable: s.d.SMS != nil,
	}, nil
}

// UpdateMe implements Service (updateMe): names, locale and, once, the
// birth date.
func (s *service) UpdateMe(ctx context.Context, a authz.Actor, p MePatch) (Me, error) {
	if err := authz.Can(ctx, a, authz.IdentityAccountUpdateOwn, ownUser(a)); err != nil {
		return Me{}, err
	}
	if p.FirstName == nil && p.LastName == nil && p.Locale == nil && p.BirthDate == nil {
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
	if p.BirthDate != nil {
		if err := checkBirthDate(*p.BirthDate, s.now()); err != nil {
			return Me{}, err
		}
	}
	var out User
	var localeChanged bool
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.users().LockUser(ctx, a.UserID)
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
		birth, err := newBirthDate(u, p.BirthDate)
		if err != nil {
			return err
		}
		if len(fields) > 0 { // UpdateUser never writes the birth date
			if err := s.users().UpdateUser(ctx, u); err != nil {
				return err
			}
		}
		if birth {
			switch set, err := s.users().SetBirthDate(ctx, u.ID, *p.BirthDate); {
			case err != nil:
				return err
			case !set: // set meanwhile: never overwritten
				return ErrBirthDateLocked
			}
			u.BirthDate, fields = p.BirthDate, append(fields, "birth_date")
		}
		out = u
		if len(fields) == 0 {
			return nil
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

// newBirthDate decides updateMe's birth_date d on u: true when the account
// has none (d is written, set once); the same date again changes nothing,
// another one is ErrBirthDateLocked.
func newBirthDate(u User, d *civil.Date) (bool, error) {
	switch {
	case d == nil || (u.BirthDate != nil && *u.BirthDate == *d):
		return false, nil
	case u.BirthDate != nil:
		return false, ErrBirthDateLocked
	}
	return true, nil
}

// ---- Users (01 §4.6)

func (s *service) summary(u User, student bool) UserSummary {
	return UserSummary{ID: u.ID, Status: u.Status, FirstName: u.FirstName, LastName: u.LastName, Locale: u.Locale,
		IsMinor: MinorProtected(u.BirthDate, student, s.now())}
}

// summaries completes us with the age facts: an account without a birth
// date is looked up among the students (MinorProtected; BE-202). Accounts
// with a birth date cost nothing more.
func (s *service) summaries(ctx context.Context, us []User) (map[uuid.UUID]UserSummary, error) {
	var unknown []uuid.UUID
	for _, u := range us {
		if u.BirthDate == nil {
			unknown = append(unknown, u.ID)
		}
	}
	students := map[uuid.UUID]bool{}
	if len(unknown) > 0 {
		ids, err := s.edu().UsersEverVerified(ctx, unknown)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			students[id] = true
		}
	}
	out := make(map[uuid.UUID]UserSummary, len(us))
	for _, u := range us {
		out[u.ID] = s.summary(u, students[u.ID])
	}
	return out, nil
}

// Get implements Users.
func (s *service) Get(ctx context.Context, id uuid.UUID) (UserSummary, error) {
	u, err := s.users().User(ctx, id)
	if err != nil {
		return UserSummary{}, err
	}
	out, err := s.summaries(ctx, []User{u})
	if err != nil {
		return UserSummary{}, err
	}
	return out[u.ID], nil
}

// GetMany implements Users.
func (s *service) GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]UserSummary, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]UserSummary{}, nil
	}
	us, err := s.users().Users(ctx, ids)
	if err != nil {
		return nil, err
	}
	return s.summaries(ctx, us)
}

// Contact implements Users (worker/notification path only).
func (s *service) Contact(ctx context.Context, id uuid.UUID) (UserContact, error) {
	u, err := s.users().User(ctx, id)
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
	u, found, err := s.users().UserByEmail(ctx, n)
	if err != nil || !found {
		return uuid.Nil(), false, err
	}
	return u.ID, true, nil
}
