package main

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
)

// orgsWiring wires the orgs module (L5; 01 §4.7). First owner: BE-017;
// membership lifecycle: BE-019. Only tickets of the orgs module edit this
// file (BE-001 R4).
//
// Seams (nil in production):
//
//	store  nil → orgs.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx     nil → the app_rw pool's TxManager; tests preset db.NopTx
//	deriver nil → crypto.Deriver over CRYPTO_TOKEN_KEYS (invitation tokens)
//	universityOrgs  searchOrgs?university_id: the orgs of a university
//	       (its own org and its clubs). directory (L6) builds after orgs
//	       and sets it in wire_directory.go (BE-020). Read at call time;
//	       nil → the filter matches nothing.
//	graduatedUsers  the graduated students among a list of users, for
//	       orgs.admin_sweep. students (L7) sets it in wire_students.go
//	       (BE-023). Read at call time; nil → the sweep checks the student
//	       verification only (student.graduated still ends the role).
//	avatarURLs  member avatars of the member list. students (L7) sets it
//	       in wire_students.go (BE-023). Read at call time; nil → no
//	       avatar_url.
//	students  tests only (the authz matrix): the student verification
//	       used instead of identity's (its stub verifies nobody until
//	       BE-018). nil in production.
//
// Consumed: c.audit.svc, c.files.svc (org logo), c.identity.svc
// (re-authentication of the R operations; Sessions.RevokeAll after a role
// change, called after the commit; the user directory: member names, the
// e-mail match of an invitation, the age of a club_admin), the identity
// module's student verification, c.notify.svc (invitation e-mails),
// hooks.clubInfo (joinClub, club sensitivity; read at call time: directory
// builds later) and hooks.openCommerce (read at call time: orders BE-040
// and ledger BE-044 append to it in their build).
//
// Provided, all set in build: svc (orgs.OrgDirectory / orgs.OrgAdmin for
// directory and later layers), hooks.memberships (Actor memberships,
// 03 §2.1), c.entitlements.orgKind (06 H-40: admin grants go to companies
// only) and the orgs KVKK provider.
type orgsWiring struct {
	store   orgs.Store
	tx      db.TxManager
	deriver *crypto.Deriver

	universityOrgs func(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error)
	graduatedUsers func(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	avatarURLs     func(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error)

	students orgs.Students

	mod orgs.Module
	svc orgs.Service // set by build
}

func (w *orgsWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = orgs.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("orgs: no store (database not opened)")
	}
	deriver := w.deriver
	if deriver == nil {
		keys, err := crypto.ParseKeyring(c.infra.platform.Crypto.TokenKeys.Reveal())
		if err != nil {
			return fmt.Errorf("orgs: CRYPTO_TOKEN_KEYS: %w", err)
		}
		if deriver, err = crypto.NewDeriver(keys); err != nil {
			return fmt.Errorf("orgs: CRYPTO_TOKEN_KEYS: %w", err)
		}
	}
	link := c.infra.cfg.Identity.LinkBaseURL // the web application origin (IDENTITY_LINK_BASE_URL)
	if c.identity.cfg != nil {
		link = c.identity.cfg.LinkBaseURL
	}
	d := orgs.Deps{
		Store: store, Tx: tx, Outbox: c.infra.outbox, Audit: c.audit.svc, Idempotency: c.infra.idempotency,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "orgs"), Deriver: deriver, LinkBaseURL: link,
		OpenCommerce: func() []orgs.OpenCommerceCheck {
			out := make([]orgs.OpenCommerceCheck, 0, len(c.hooks.openCommerce))
			for _, h := range c.hooks.openCommerce {
				out = append(out, orgs.OpenCommerceCheck(h))
			}
			return out
		},
		UniversityOrgs: func(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error) {
			if w.universityOrgs == nil {
				return nil, nil
			}
			return w.universityOrgs(ctx, universityID)
		},
		Graduated: func(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
			if w.graduatedUsers == nil {
				return map[uuid.UUID]bool{}, nil
			}
			return w.graduatedUsers(ctx, userIDs)
		},
		Avatars: func(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
			if w.avatarURLs == nil {
				return map[uuid.UUID]string{}, nil
			}
			return w.avatarURLs(ctx, userIDs)
		},
		Clubs: func(ctx context.Context, clubOrgID uuid.UUID) (orgs.ClubFacts, bool, error) {
			info := c.hooks.clubInfo // directory (BE-020) fills it in its build, after orgs
			if info == nil {
				return orgs.ClubFacts{}, false, nil // T-77: no provider → joinClub 404, clubs count as sensitive
			}
			h, found, err := info(ctx, clubOrgID)
			if err != nil || !found {
				return orgs.ClubFacts{}, false, err
			}
			return orgs.ClubFacts{UniversityID: h.UniversityID, JoinPolicy: h.JoinPolicy, Sensitive: h.Sensitive}, true, nil
		},
	}
	if svc := c.identity.svc; svc != nil {
		d.Reauth, d.Users = svc, svc
		d.RevokeSessions = func(ctx context.Context, userID uuid.UUID) (int, error) {
			return svc.RevokeAll(ctx, userID, identity.RevokeRoleChange)
		}
		d.Students = studentsSeam{w: w, fallback: c.identity.mod.Students}
	}
	if c.notify.svc != nil {
		d.Mailer = c.notify.svc
	}
	if c.files.svc != nil {
		d.Files = c.files.svc
	}
	mod, err := orgs.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc = mod, mod.Service
	c.privacy = append(c.privacy, mod.Privacy)
	svc := mod.Service
	c.hooks.memberships = func(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error) {
		return svc.MembershipsOf(ctx, userID)
	}
	c.entitlements.orgKind = func(ctx context.Context, orgID uuid.UUID) (string, bool, error) {
		o, err := svc.Get(ctx, orgID)
		switch {
		case errors.Is(err, orgs.ErrNotFound):
			return "", false, nil
		case err != nil:
			return "", false, err
		}
		return string(o.Kind), true, nil
	}
	return nil
}

// studentsSeam answers from the test seam when one is set, else from
// identity's student verification (read at call time).
type studentsSeam struct {
	w        *orgsWiring
	fallback identity.StudentVerification
}

func (s studentsSeam) src() orgs.Students {
	if s.w.students != nil {
		return s.w.students
	}
	return s.fallback
}

func (s studentsSeam) Active(ctx context.Context, userID uuid.UUID) (identity.EduVerification, bool, error) {
	if src := s.src(); src != nil {
		return src.Active(ctx, userID)
	}
	return identity.EduVerification{}, false, nil
}

func (s studentsSeam) ActiveMany(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]identity.EduVerification, error) {
	if src := s.src(); src != nil {
		return src.ActiveMany(ctx, userIDs)
	}
	return map[uuid.UUID]identity.EduVerification{}, nil
}

// tasks: org_invitation.created → orgs.send_invitation_email;
// user.edu_verification_lapsed → orgs.end_lapsed_admin_roles;
// student.graduated → orgs.end_graduated_admin_roles;
// club.sensitivity_changed → orgs.purge_sensitive_memberships; orgs.admin_sweep
// (01:00, default) and orgs.invitations_expire (:15, low; retention
// org_invitation) (01 §6.2, §7). orgs.ended_memberships_purge is BE-057's.
func (w *orgsWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	invitations := c.retention("org_invitation", 0, orgs.DefaultInvitationRetention)
	for _, j := range w.mod.Jobs(orgs.Retention(invitations)) {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 24 orgs operations of BE-017 and BE-019 (04 §2.2, joinClub of
// §2.3); w.svc is nil under `koro openapi`, which Register tolerates.
func (w *orgsWiring) routes(c *container, api huma.API) { orgs.Register(api, w.svc) }
