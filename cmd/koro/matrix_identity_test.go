package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/civil"
	"koro/internal/platform/config"
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
			Persona: identity.PersonaCompany, Locale: identity.LocaleTR,
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
		// The persona's platform roles in effect, as bootstrap rows (02 §5.7),
		// so the store agrees with the actor (last admin, talent limit).
		for _, r := range p.Actor.PlatformRoles {
			ra := identity.RoleAssignment{ID: uuid.NewV7(), UserID: u.ID, Role: r, Reason: "bootstrap", Status: identity.RoleActive,
				RequestedAt: now, GrantedAt: &now}
			if err := w.store.InsertRoleAssignment(ctx, ra); err != nil {
				return fmt.Errorf("role %s of %s: %w", r, p.Name, err)
			}
		}
		if err := w.matrixTOTP(ctx, u); err != nil {
			return fmt.Errorf("totp %s: %w", p.Name, err)
		}
		// BE-018: a verified student persona has its verification in the store
		// too (getMyEduVerification, renewals), at the persona's university.
		if p.Actor.IsVerifiedStudent() {
			uni, exp, addr := *p.Actor.UniversityID, now.AddDate(1, 0, 0), p.Name+"@matrix.edu.tr"
			v := identity.EduRecord{ID: uuid.NewV7(), UserID: u.ID, Method: identity.EduMethodEmail, EduEmail: addr,
				EduEmailNormalized: addr, EmailDomain: "matrix.edu.tr", UniversityID: &uni, Status: identity.EduVerified,
				VerifiedAt: &now, ExpiresAt: &exp}
			if err := w.store.InsertEduVerification(ctx, v); err != nil {
				return fmt.Errorf("student verification %s: %w", p.Name, err)
			}
		}
	}
	// The matrix directory: matrix.edu.tr and its subdomains are uni_a.
	w.domains = func(_ context.Context, domain string) (uuid.UUID, bool, error) {
		if domain == "matrix.edu.tr" || strings.HasSuffix(domain, ".matrix.edu.tr") {
			return authztest.UniversityID("uni_a"), true, nil
		}
		return uuid.Nil(), false, nil
	}
	fixtures := fx.Of("identity")
	slices.SortStableFunc(fixtures, func(a, b fixture) int { // accounts and files before what refers to them
		return cmp.Compare(fixtureRank(a.Kind), fixtureRank(b.Kind))
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
			}
			if u.Status == identity.StatusSuspended {
				u.EmailVerifiedAt, u.SuspendedAt, u.SuspensionReason = &now, &now, "matrix fixture"
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
		case "mfa_challenge":
			owner, err := fx.Ref(f, "user")
			if err != nil {
				return err
			}
			if err := identity.SeedChallenge(ctx, w.challenges, id, owner, identity.ClientMobile, matrixClock.Add(identity.ChallengeTTL), matrixClock); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "role_assignment":
			if err := w.matrixRole(ctx, fx, f, id, now); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "edu_document_file":
			if err := w.matrixEduFile(ctx, fx, f, id, now); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "edu_verification":
			owner, err := fx.Ref(f, "user")
			if err != nil {
				return err
			}
			file, err := fx.Ref(f, "file")
			if err != nil {
				return err
			}
			uni := authztest.UniversityID("uni_a")
			v := identity.EduRecord{ID: id, UserID: owner, Method: identity.EduMethodDocument, UniversityID: &uni,
				DocumentFileID: &file, Status: identity.EduPending}
			if err := w.store.InsertEduVerification(ctx, v); err != nil {
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

// fixtureRank orders the identity fixtures: accounts, then student
// document files, then what refers to them.
func fixtureRank(kind string) int {
	switch kind {
	case "user":
		return 0
	case "edu_document_file":
		return 1
	}
	return 2
}

// matrixEduFile seeds an edu_document file (BE-018) into the files store and
// object storage of the container: uploaded by user, verified, skipped by
// the no-op scanner (served while FILES_SCAN_REQUIRED=false), attached to
// the verification attached_to when given.
func (w *identityWiring) matrixEduFile(ctx context.Context, fx *matrixFixtures, f fixture, id uuid.UUID, now time.Time) error {
	by, err := fx.Ref(f, "user")
	if err != nil {
		return err
	}
	if w.late == nil {
		return errors.New("identity wiring was not built")
	}
	mem, err := w.late.files.memObjects()
	if err != nil {
		return err
	}
	size := int64(len(matrixFilePDF))
	row := files.File{
		ID: id, Purpose: files.PurposeEduDocument, OwnerUserID: &by, UploadedBy: &by, Bucket: testBuckets.Files,
		StorageKey: "matrix/" + f.Name, DeclaredMIME: files.MIMEPDF, SizeBytes: &size, SHA256: make([]byte, 32),
		Status: files.StatusActive, ScanStatus: files.ScanSkipped,
	}
	if _, ok := f.Fields["attached_to"]; ok {
		owner, err := fx.Ref(f, "attached_to")
		if err != nil {
			return err
		}
		module := "identity"
		row.AttachedModule, row.AttachedEntityID, row.AttachedAt = &module, &owner, &now
	}
	mem.Upload(row.Bucket, row.StorageKey, matrixFilePDF, files.MIMEPDF)
	return w.late.files.store.Insert(ctx, row)
}

// matrixTOTPSecret is the TOTP secret of every matrix account when the
// identity_mfa config enrolls them; its code at the container clock
// (2026-10-03T09:00:00Z) is matrixTOTPCode (the bodies hold it).
const (
	matrixTOTPSecret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	matrixTOTPCode   = "498845"
)

// matrixMFAModes is the identity_mfa config of each matrix application
// (keyed by its wiring; set by matrixConfig before build):
//
//	none      no account has TOTP (default; R proofs are passwords)
//	enrolled  every account has an unconfirmed enrollment (confirmTotp)
//	enabled   every account has TOTP on (verifyMfa, disableTotp, …; R
//	          proofs are TOTP codes; MFA logins answer a challenge)
var matrixMFAModes sync.Map

// matrixConfig implements matrixConfigurer: identity_mfa.
func (w *identityWiring) matrixConfig(_ *config.Modules, key string, v any) (bool, error) {
	if key != "identity_mfa" {
		return false, nil
	}
	mode, _ := v.(string)
	switch mode {
	case "none", "enrolled", "enabled":
	default:
		return true, fmt.Errorf("identity_mfa %v: want none, enrolled or enabled", v)
	}
	matrixMFAModes.Store(w, mode)
	return true, nil
}

// matrixTOTP gives u the TOTP state of the identity_mfa config.
func (w *identityWiring) matrixTOTP(ctx context.Context, u identity.User) error {
	mode, _ := matrixMFAModes.Load(w)
	if mode == nil || mode == "none" {
		return nil
	}
	sealed, err := w.secrets.Seal(ctx, identity.TOTPSecretPurpose(u.ID), []byte(matrixTOTPSecret))
	if err != nil {
		return err
	}
	if err := w.store.PutTOTP(ctx, identity.TOTP{UserID: u.ID, SecretEnc: sealed}); err != nil {
		return err
	}
	if mode != "enabled" {
		return nil
	}
	// Confirmed two steps before the container clock: the code of "now" is fresh.
	step := matrixClock.Unix()/30 - 2
	if _, err := w.store.ConfirmTOTP(ctx, u.ID, step, u.CreatedAt); err != nil {
		return err
	}
	cur, err := w.store.User(ctx, u.ID)
	if err != nil {
		return err
	}
	cur.MFAEnabled = true
	return w.store.UpdateUser(ctx, cur)
}

// matrixClock is the fake clock of newTestContainer.
var matrixClock = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// matrixRole seeds a role_assignment fixture: user, role, status (pending
// | active), requested_by and, for an active privileged role, approved_by.
func (w *identityWiring) matrixRole(ctx context.Context, fx *matrixFixtures, f fixture, id uuid.UUID, now time.Time) error {
	user, err := fx.Ref(f, "user")
	if err != nil {
		return err
	}
	req, err := fx.Ref(f, "requested_by")
	if err != nil {
		return err
	}
	role, _ := f.Fields["role"].(string)
	status, _ := f.Fields["status"].(string)
	ra := identity.RoleAssignment{ID: id, UserID: user, Role: authz.PlatformRole(role), Reason: "matrix fixture",
		Status: identity.RoleStatus(status), RequestedBy: &req, RequestedAt: now}
	if ra.Status == identity.RoleActive {
		ra.GrantedAt = &now
		if _, ok := f.Fields["approved_by"]; ok {
			by, err := fx.Ref(f, "approved_by")
			if err != nil {
				return err
			}
			ra.ApprovedBy, ra.ApprovedAt = &by, &now
		}
	}
	return w.store.InsertRoleAssignment(ctx, ra)
}
