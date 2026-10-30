package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
)

// BootstrapJob is the audit system actor of `koro seed bootstrap-admin`.
const BootstrapJob = "identity.bootstrap_admin"

// bootstrapAdmins is the number of platform admins the bootstrap opens:
// two, so that the four-eyes rule (06 H-17) does not deadlock (03 §6.1:
// going live needs at least two active platform_admins).
const bootstrapAdmins = 2

// AdminSeed is one admin of `koro seed bootstrap-admin`.
type AdminSeed struct {
	Email     string
	FirstName string
	LastName  string
}

// BootstrapOutcome reports what the bootstrap did for one admin.
type BootstrapOutcome struct {
	UserID      uuid.UUID
	UserCreated bool // a new account (active, verified, no password: set it with the password reset)
	RoleGranted bool // false: the admin already held platform_admin (idempotent re-run)
}

// BootstrapDeps are the dependencies of the bootstrap (CLI only).
type BootstrapDeps struct {
	Store Store
	Tx    db.TxManager
	Audit AuditLogger
	Clock clock.Clock
	Log   *slog.Logger
}

// Bootstrapper opens the first two platform_admin assignments (02 §5.7:
// requested_by NULL, reason 'bootstrap', active without a second approver
// — the only privileged rows of that shape). It is reachable from the CLI
// only, never over HTTP.
type Bootstrapper struct{ d BootstrapDeps }

// NewBootstrapper returns the bootstrapper.
func NewBootstrapper(d BootstrapDeps) (*Bootstrapper, error) {
	if d.Store == nil || d.Tx == nil || d.Audit == nil || d.Clock == nil || d.Log == nil {
		return nil, errors.New("identity: BootstrapDeps incomplete")
	}
	return &Bootstrapper{d: d}, nil
}

func refuse(format string, args ...any) error { return ErrBootstrap.Wrap(fmt.Errorf(format, args...)) }

// Run bootstraps exactly two admins, in one serializable transaction. It is
// idempotent: re-running it with the same admins changes nothing. It
// refuses when another user already holds platform_admin (then the
// four-eyes grant flow applies) or when an account is unusable.
func (b *Bootstrapper) Run(ctx context.Context, admins []AdminSeed) ([]BootstrapOutcome, error) {
	if len(admins) != bootstrapAdmins {
		return nil, refuse("exactly %d admins are required, got %d", bootstrapAdmins, len(admins))
	}
	type seed struct {
		email, normalized, first, last string
	}
	seeds := make([]seed, len(admins))
	for i, a := range admins {
		email, normalized, err := parseEmail(a.Email, fmt.Sprintf("admin[%d].email", i))
		if err != nil {
			return nil, err
		}
		first, err := cleanName(a.FirstName, fmt.Sprintf("admin[%d].first_name", i))
		if err != nil {
			return nil, err
		}
		last, err := cleanName(a.LastName, fmt.Sprintf("admin[%d].last_name", i))
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(seeds[:i], func(s seed) bool { return s.normalized == normalized }) {
			return nil, refuse("the two admins must have different e-mail addresses")
		}
		seeds[i] = seed{email, normalized, first, last}
	}

	var out []BootstrapOutcome
	err := b.d.Tx.WithSerializableTx(ctx, func(ctx context.Context) error {
		out = out[:0]
		now := b.d.Clock.Now().UTC().Truncate(time.Microsecond)
		// Read everything before writing anything: a refusal leaves no trace.
		found := make([]*User, len(seeds))
		var known []uuid.UUID
		for i, sd := range seeds {
			u, ok, err := b.users().UserByEmail(ctx, sd.normalized)
			if err != nil {
				return err
			}
			if ok {
				if u.Status != StatusActive && u.Status != StatusPendingEmail {
					return refuse("admin %d: the account is %s", i+1, u.Status)
				}
				found[i], known = &u, append(known, u.ID)
			}
		}
		holders, err := b.roles().UsersWithRole(ctx, authz.PlatformAdmin)
		if err != nil {
			return err
		}
		for _, h := range holders {
			if !slices.Contains(known, h) {
				return refuse("platform admins already exist: grant the role with the four-eyes flow")
			}
		}
		for i, sd := range seeds {
			o := BootstrapOutcome{}
			switch u := found[i]; {
			case u == nil:
				nu := User{
					ID: uuid.NewV7(), Status: StatusActive, Email: sd.email, EmailNormalized: sd.normalized, EmailVerifiedAt: &now,
					FirstName: sd.first, LastName: sd.last, Persona: PersonaPlatform, Locale: LocaleTR,
				}
				if err := b.users().InsertUser(ctx, nu); err != nil {
					return err
				}
				o.UserID, o.UserCreated = nu.ID, true
				if err := b.d.Audit.Log(ctx, audit.Event{Action: string(authz.IdentityAccountRegister), Outcome: audit.Success,
					SystemJob: BootstrapJob, ResourceType: resourceUser, ResourceID: &nu.ID,
					Metadata: map[string]any{"signup_persona": string(PersonaPlatform), "bootstrap": true}}); err != nil {
					return err
				}
			case u.Status == StatusPendingEmail:
				// The operator vouches for the address.
				u.Status, u.EmailVerifiedAt = StatusActive, &now
				if err := b.users().UpdateUser(ctx, *u); err != nil {
					return err
				}
				o.UserID = u.ID
			default:
				o.UserID = u.ID
			}
			out = append(out, o)
		}
		for i := range out {
			as, err := b.roles().RoleAssignmentsOf(ctx, out[i].UserID)
			if err != nil {
				return err
			}
			open := slices.IndexFunc(as, func(a RoleAssignment) bool {
				return a.Role == authz.PlatformAdmin && (a.Status == RolePending || a.Status.InEffect())
			})
			if open >= 0 {
				if as[open].Status == RolePending {
					return refuse("admin %d has a pending platform_admin request", i+1)
				}
				continue // already an admin: idempotent
			}
			ra := RoleAssignment{
				ID: uuid.NewV7(), UserID: out[i].UserID, Role: authz.PlatformAdmin, Reason: bootstrapReason,
				Status: RoleActive, RequestedAt: now, GrantedAt: &now,
			}
			if err := b.roles().InsertRoleAssignment(ctx, ra); err != nil {
				return err
			}
			// 03 §2.2-6: a role change closes the user's sessions.
			if _, err := b.sessions().RevokeUserSessions(ctx, out[i].UserID, nil, now, RevokeRoleChange); err != nil {
				return err
			}
			uid := out[i].UserID
			if err := b.d.Audit.Log(ctx, audit.Event{Action: string(authz.IdentityPlatformRoleGrant), Outcome: audit.Success,
				SystemJob: BootstrapJob, ResourceType: resourceRole, ResourceID: &ra.ID, Reason: bootstrapReason,
				Metadata: map[string]any{"role": string(authz.PlatformAdmin), "bootstrap": true, audit.MetaSubjectUserID: uid.String()}}); err != nil {
				return err
			}
			out[i].RoleGranted = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, o := range out {
		b.d.Log.LogAttrs(ctx, slog.LevelInfo, "bootstrap admin", slog.String("user_id", o.UserID.String()),
			slog.Bool("user_created", o.UserCreated), slog.Bool("role_granted", o.RoleGranted))
	}
	return out, nil
}
