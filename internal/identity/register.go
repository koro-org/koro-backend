package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"strconv"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/civil"
	"koro/internal/platform/problem"
)

// Registration is the body of registerAccount (04 §2.1).
type Registration struct {
	Email     string
	Password  string
	FirstName string
	LastName  string
	BirthDate *civil.Date // mandatory for students (age gate, 02 §5.1)
	Persona   Persona     // student | company | university | club
	Locale    string
	// PrivacyNoticeID is the privacy notice the client showed. It is
	// recorded as notice.shown in the audit log; it is never "accepted"
	// (02 §4.2: aydınlatma is shown, not consented to).
	PrivacyNoticeID uuid.UUID
	// Consents are the optional, unticked-by-default consents the user
	// gave on the registration page: one row per purpose, never bundled
	// with the account and never part of the e-mail verification
	// (BE-012, 06 H-42).
	Consents []ConsentGrant
	// Client is the X-Koro-Client of the request (the consent channel).
	Client ClientKind
}

// ConsentGrant is one consent given at registration.
type ConsentGrant struct {
	Purpose         consent.Purpose
	NoticeVersionID uuid.UUID
	TextSHA256      []byte
}

// String keeps the password out of %v.
func (r Registration) String() string { return "identity.Registration{" + string(r.Persona) + "}" }

// personaAudience maps a signup persona to its privacy notice audience.
var personaAudience = map[Persona]consent.Audience{
	PersonaStudent: consent.AudienceStudent, PersonaCompany: consent.AudienceCompany,
	PersonaUniversity: consent.AudienceUniversity, PersonaClub: consent.AudienceClub,
}

// errRegistrationRace ends the transaction of a registration whose address
// was taken by a concurrent request (users_email_uq): the answer is the
// same 202.
var errRegistrationRace = errors.New("identity: address taken concurrently")

// Register implements Service (registerAccount). A well-formed request
// returns nil whether the address is new or taken: the response and its
// cost are the same (one argon2id hash in both paths; ASVS 6.3.8). A taken
// address gets a security notice (or, while still unverified, a fresh
// verification e-mail) instead of a second account.
func (s *service) Register(ctx context.Context, a authz.Actor, in Registration) error {
	if err := authz.Can(ctx, a, authz.IdentityAccountRegister, publicUser); err != nil {
		return err
	}
	now := s.now()
	email, normalized, err := parseEmail(in.Email, "body.email")
	if err != nil {
		return err
	}
	first, err := cleanName(in.FirstName, "body.first_name")
	if err != nil {
		return err
	}
	last, err := cleanName(in.LastName, "body.last_name")
	if err != nil {
		return err
	}
	audience, ok := personaAudience[in.Persona]
	if !ok {
		return invalid(problem.ErrValidation, "body.signup_persona", "must be student, company, university or club")
	}
	if err := checkLocale(in.Locale, "body.locale"); err != nil {
		return err
	}
	switch {
	case in.BirthDate == nil && in.Persona == PersonaStudent:
		return invalid(ErrBirthDateRequired, "body.birth_date", "is required for students")
	case in.BirthDate != nil:
		if err := checkBirthDate(*in.BirthDate, now); err != nil {
			return err
		}
	}
	if err := s.checkPrivacyNotice(ctx, in.PrivacyNoticeID, audience); err != nil {
		return err
	}
	if err := s.checkConsents(ctx, in.Consents); err != nil {
		return err
	}
	if err := checkPassword(in.Password, false, passwordContext{Email: email, FirstName: first, LastName: last}, "body.password"); err != nil {
		return err
	}
	if s.d.Hasher == nil {
		return ErrUnavailable
	}
	// Hash before looking the address up: both paths pay for one argon2id.
	hash, err := s.d.Hasher.Hash(ctx, in.Password)
	if err != nil {
		return err
	}

	var created *User
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		existing, found, err := s.d.Store.UserByEmail(ctx, normalized)
		if err != nil {
			return err
		}
		if found {
			return s.registerTaken(ctx, existing)
		}
		u := User{
			ID: uuid.NewV7(), Status: StatusPendingEmail, Email: email, EmailNormalized: normalized,
			PasswordHash: hash, FirstName: first, LastName: last, BirthDate: in.BirthDate,
			Persona: in.Persona, Locale: in.Locale,
		}
		if err := s.d.Store.InsertUser(ctx, u); err != nil {
			if errors.Is(err, ErrEmailTaken) {
				return errRegistrationRace
			}
			return err
		}
		meta := map[string]any{"signup_persona": string(u.Persona), "consents": len(in.Consents)}
		if err := s.d.Audit.Log(ctx, byUser(event(authz.IdentityAccountRegister, audit.Success, resourceUser, u.ID, meta), u.ID)); err != nil {
			return err
		}
		// 02 §4.2 / 06 H-42: the privacy notice shown on the registration
		// page, in the registration transaction.
		shown := event(auditNoticeShown, audit.Success, resourceNotice, in.PrivacyNoticeID,
			map[string]any{"kind": string(consent.KindPrivacyNotice), "context": "registration"})
		if err := s.d.Audit.Log(ctx, byUser(shown, u.ID)); err != nil {
			return err
		}
		channel := consent.ChannelWeb
		if in.Client == ClientMobile {
			channel = consent.ChannelMobile
		}
		for _, c := range in.Consents {
			nid := c.NoticeVersionID
			_, err := s.d.Consent.Record(ctx, consent.RecordInput{
				UserID: u.ID, Purpose: c.Purpose, Action: consent.Granted, NoticeVersionID: &nid,
				TextSHA256: c.TextSHA256, Channel: channel, Minor: u.IsMinor(now),
			})
			if pe, ok := errors.AsType[*problem.Error](err); ok && (pe.Kind() == problem.Invalid || pe.Kind() == problem.Conflict) {
				// A consent rule the request could not see (inactive
				// purpose, 03 k12 for a minor, scope): the account is
				// created without this consent. Failing would answer
				// differently for a taken address (enumeration); not
				// recording is the safe direction (no consent = no
				// commercial message). The user can grant it later.
				s.d.Log.LogAttrs(ctx, slog.LevelWarn, "registration consent not recorded",
					slog.String("user_id", u.ID.String()), slog.String("purpose", string(c.Purpose)), slog.String("code", pe.Code()))
				continue
			}
			if err != nil {
				return err
			}
		}
		// BE-097: a student's .edu.tr address is proven for the student
		// verification by this first token too (edu_signup.go).
		if _, err := s.issueToken(ctx, u, PurposeAccountEmail, signupEduTarget(u)); err != nil {
			return err
		}
		created = &u
		return nil
	})
	switch {
	case errors.Is(err, errRegistrationRace):
		return nil
	case err != nil:
		return err
	}
	if created != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "account registered", slog.String("user_id", created.ID.String()),
			slog.String("signup_persona", string(created.Persona)))
	}
	return nil
}

// registerTaken handles a registration for an existing address: no second
// account, no answer that differs from a new one. The owner learns about
// it by e-mail (04 §2.1); an unverified owner gets a fresh verification
// e-mail instead (the likely case: the first one was lost).
func (s *service) registerTaken(ctx context.Context, u User) error {
	switch u.Status {
	case StatusPendingEmail:
		if _, err := s.issueToken(ctx, u, PurposeAccountEmail, ""); err != nil {
			return err
		}
	case StatusActive:
		s.notifyUser(ctx, u.ID, NoticeSignupAttempt)
	default: // suspended: nothing is sent
	}
	return s.d.Audit.Log(ctx, event(authz.IdentityAccountRegister, audit.Denied, resourceUser, u.ID,
		map[string]any{"result": "email_taken"}))
}

// checkConsents validates the registration consents against the consent
// texts before the address is looked up, so that a taken address and a new
// one fail (or pass) alike: no purpose twice, and every proof names the
// current consent text of its purpose with the digest of its body.
func (s *service) checkConsents(ctx context.Context, cs []ConsentGrant) error {
	seen := map[consent.Purpose]bool{}
	for i, c := range cs {
		loc := "body.consents[" + strconv.Itoa(i) + "]"
		switch {
		case seen[c.Purpose]:
			return invalid(ErrDuplicateConsent, loc+".purpose_code", "is listed twice")
		case c.NoticeVersionID == uuid.Nil() || len(c.TextSHA256) == 0:
			return invalid(consent.ErrProofRequired, loc, "notice_version_id and text_sha256 are required")
		}
		seen[c.Purpose] = true
		n, err := s.d.Notices.NoticeByID(ctx, c.NoticeVersionID)
		switch {
		case isNotFound(err):
			return invalid(consent.ErrNoticeNotCurrent, loc+".notice_version_id", "is not a current consent text")
		case err != nil:
			return err
		case n.Kind != consent.KindConsentText || n.Purpose != c.Purpose:
			return invalid(consent.ErrNoticeMismatch, loc+".notice_version_id", "is not the consent text of this purpose")
		case !n.Published:
			return invalid(consent.ErrNoticeNotCurrent, loc+".notice_version_id", "is not a current consent text")
		case subtle.ConstantTimeCompare(n.BodySHA256, c.TextSHA256) != 1:
			return invalid(consent.ErrTextMismatch, loc+".text_sha256", "does not match the consent text")
		}
	}
	return nil
}

// checkPrivacyNotice requires the current privacy notice of the persona's
// audience (or of audience all).
func (s *service) checkPrivacyNotice(ctx context.Context, id uuid.UUID, audience consent.Audience) error {
	bad := invalid(ErrPrivacyNoticeInvalid, "body.privacy_notice_version_id", "must be the current privacy notice")
	if id == uuid.Nil() {
		return bad
	}
	n, err := s.d.Notices.NoticeByID(ctx, id)
	switch {
	case isNotFound(err):
		return bad
	case err != nil:
		return err
	}
	if n.Kind != consent.KindPrivacyNotice || !n.Published || (n.Audience != audience && n.Audience != consent.AudienceAll) {
		return bad
	}
	return nil
}
