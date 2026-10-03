package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/students"
)

// studentsWiring wires the students module (L7; 01 §4.9). First owner:
// BE-023. Only tickets of the students module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store  nil → students.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx     nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumed (all built in lower layers): c.audit.svc, c.consent.svc
// (Recorder in the privacy transaction, Checker for the nightly
// reconciliation), c.files.svc (avatar: Get, Attach, PresignGet; Delete of
// a replaced or erased avatar), c.identity.svc (user directory: age and
// account status), the identity module's student verification
// (eligibility, graduation), c.orgs.svc (club memberships, club members)
// and c.directory.svc (names, department and skill validation, sensitive
// clubs).
//
// Provided, set in build: svc (students.Service: Profiles for feed,
// outreach, talent), the students KVKK provider, and the late-bound reads
// of orgs: c.orgs.graduatedUsers (orgs.admin_sweep, T-210) and
// c.orgs.avatarURLs (member lists, 04 §2.2).
type studentsWiring struct {
	store students.Store
	tx    db.TxManager

	// late is the container, kept for the matrix fixtures (the consent
	// text of the discoverable variant lives in the consent store).
	late *container

	mod students.Module
	svc students.Service // set by build
}

func (w *studentsWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = students.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("students: no store (database not opened)")
	}
	d := students.Deps{
		Store: store, Tx: tx, Outbox: c.infra.outbox, Audit: c.audit.svc, Idempotency: c.infra.idempotency,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "students"),
	}
	if svc := c.consent.svc; svc != nil {
		d.Consent, d.Consents = svc, svc
	}
	if svc := c.directory.svc; svc != nil {
		d.Catalog = svc
	}
	if svc := c.orgs.svc; svc != nil {
		d.Orgs = svc
	}
	if svc := c.identity.svc; svc != nil {
		d.Users = svc
	}
	if v := c.identity.mod.Students; v != nil {
		d.Verifications = v
	}
	if fs := c.files.svc; fs != nil {
		d.Files = fs
		d.RemoveFile = func(ctx context.Context, a authz.Actor, id uuid.UUID) error { return fs.Delete(ctx, a, id) }
	}
	mod, err := students.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.late = mod, mod.Service, c
	c.privacy = append(c.privacy, mod.Privacy)
	svc := mod.Service
	c.orgs.graduatedUsers = svc.GraduatedAmong
	c.orgs.avatarURLs = svc.AvatarURLs
	return nil
}

// tasks: membership.started → students.add_club_key, membership.ended →
// students.remove_club_key, user.edu_verified → students.apply_edu_verified,
// user.edu_verification_lapsed → students.apply_edu_lapsed,
// club.sensitivity_changed → students.drop_sensitive_club_key (T-80);
// students.audience_refresh (02:00, low) and students.graduation_check
// (02:15, default) (01 §6.2, §7).
func (w *studentsWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 9 students operations of BE-023 (04 §2.4); w.svc is nil
// under `koro openapi`, which Register tolerates.
func (w *studentsWiring) routes(c *container, api huma.API) { students.Register(api, w.svc) }
