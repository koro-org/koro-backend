package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/platform/civil"
	"koro/internal/platform/token"
)

// matrixEmail is the address of a matrix account.
func matrixEmail(name string) string { return name + "@matrix.koro.test" }

// matrixToken is the literal e-mail token of a token fixture (the bodies
// hold it): the name without underscores, upper case, padded with A to 52
// base32 characters.
func matrixToken(name string) string {
	t := strings.ToUpper(strings.ReplaceAll(name, "_", ""))
	return t + strings.Repeat("A", 52-len(t))
}

// matrixFixtures seeds the identity accounts of the matrix (03 §7.3): one
// active account per persona and helper user (password
// matrixReauthPassword, a mobile session with the persona's session ID)
// plus fixtures/identity.yaml.
func (w *identityWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	adult, _ := civil.New(2000, time.March, 14)
	minor := civil.Today(now).AddDays(-17 * 365)
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if !p.Actor.IsUser() {
			continue
		}
		u := identity.User{
			ID: p.User(), Status: identity.StatusActive, Email: matrixEmail(p.Name), EmailNormalized: matrixEmail(p.Name),
			EmailVerifiedAt: &now, PasswordHash: testPasswordHash(), FirstName: "Ayla", LastName: p.Name,
			Persona: identity.PersonaCompany, Locale: identity.LocaleTR, CreatedAt: now, UpdatedAt: now,
		}
		switch {
		case len(p.Actor.PlatformRoles) > 0:
			u.Persona = identity.PersonaPlatform
		case p.Actor.IsVerifiedStudent() && p.Actor.IsMinor:
			u.Persona, u.BirthDate = identity.PersonaStudent, &minor
		case p.Actor.IsVerifiedStudent():
			b := adult
			u.Persona, u.BirthDate = identity.PersonaStudent, &b
		}
		if err := w.store.InsertUser(ctx, u); err != nil {
			return fmt.Errorf("account %s: %w", p.Name, err)
		}
		if err := insertMatrixSession(ctx, w.store, p.Actor.SessionID, u.ID, now); err != nil {
			return fmt.Errorf("session %s: %w", p.Name, err)
		}
	}
	fixtures := fx.Of("identity")
	slices.SortStableFunc(fixtures, func(a, b fixture) int { // accounts before what refers to them
		return cmp.Compare(boolInt(a.Kind != "user"), boolInt(b.Kind != "user"))
	})
	for _, f := range fixtures {
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		switch f.Kind {
		case "user":
			email, _ := f.Fields["email"].(string)
			status, _ := f.Fields["status"].(string)
			u := identity.User{
				ID: id, Status: identity.Status(status), Email: email, EmailNormalized: email, PasswordHash: testPasswordHash(),
				FirstName: "Ayla", LastName: f.Name, Persona: identity.PersonaCompany, Locale: identity.LocaleTR,
				CreatedAt: now, UpdatedAt: now,
			}
			if err := w.store.InsertUser(ctx, u); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "session":
			owner, err := fx.Ref(f, "user")
			if err != nil {
				return err
			}
			if err := insertMatrixSession(ctx, w.store, id, owner, now); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "email_token":
			owner, err := fx.Ref(f, "user")
			if err != nil {
				return err
			}
			purpose, _ := f.Fields["purpose"].(string)
			target, _ := f.Fields["target_email"].(string)
			t := identity.EmailToken{
				ID: id, UserID: owner, Purpose: identity.TokenPurpose(purpose), TargetEmail: target, KeyID: "test1",
				TokenHash: token.Hash(matrixToken(f.Name)), ExpiresAt: now.Add(365 * 24 * time.Hour), CreatedAt: now,
			}
			if err := w.store.InsertEmailToken(ctx, t); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}

func insertMatrixSession(ctx context.Context, s identity.Store, id, userID uuid.UUID, now time.Time) error {
	_, hash, err := token.New()
	if err != nil {
		return err
	}
	return s.InsertSession(ctx, identity.Session{
		ID: id, UserID: userID, TokenHash: hash, ClientKind: identity.ClientMobile, CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(365 * 24 * time.Hour), AbsoluteExpiresAt: now.Add(730 * 24 * time.Hour), DeviceLabel: "Matris",
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
