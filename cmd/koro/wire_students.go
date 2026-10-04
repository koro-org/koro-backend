package main

import (
	"context"
	"errors"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// studentsWiring wires the students module (L7; 01 §4.9). First owner:
// BE-023, then BE-025. Only tickets of the students module edit this file
// (BE-001 R4).
//
// Seams (nil in production):
//
//	store  nil → students.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx     nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Late binding (filled by a higher layer, read at call time):
//
//	registrations  the events reader of the member card's
//	       club_event_registrations_count and the admin view's
//	       registrations[] (04 §V.4, §V.5). events (L8) sets it in
//	       wire_events.go (BE-025 follow-up). nil → both are absent.
//
// Consumed (built in lower layers): c.audit.svc, c.consent.svc (Recorder
// in the privacy transaction, Checker for the nightly reconciliation),
// c.files.svc (avatar and CVs: Get, Attach, PresignGet; Delete of a
// replaced, deleted, rejected or erased file), c.identity.svc (user
// directory: age and account status; AdminUser: the account part of the
// admin view), the identity module's student verification (eligibility,
// graduation), c.orgs.svc (club memberships, club members, GetMember of
// the member card) and c.directory.svc (names, department and skill
// validation, sensitive clubs). c.kvkk.svc (L13, built later) is read at
// call time for the dpo's data subject request (03 k11).
//
// Provided, set in build: svc (students.Service: Profiles for feed,
// outreach, talent; Audience for outreach), the students KVKK provider,
// and the late-bound reads of orgs: c.orgs.graduatedUsers
// (orgs.admin_sweep, T-210) and c.orgs.avatarURLs (member lists, 04 §2.2).
type studentsWiring struct {
	store students.Store
	tx    db.TxManager

	// registrations is the events reader (late binding, see above).
	registrations students.Registrations

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
		d.Orgs, d.Members = svc, svc
	}
	if svc := c.identity.svc; svc != nil {
		d.Users, d.Accounts = svc, svc
	}
	d.DSRs = studentsDSRs{c: c}
	d.Registrations = func() students.Registrations { return w.registrations }
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
// club.sensitivity_changed → students.drop_sensitive_club_key (T-80),
// file.rejected → students.drop_rejected_file (T-176: a rejected CV or
// avatar loses its reference and its file); students.audience_refresh
// (02:00, low) and students.graduation_check (02:15, default) (01 §6.2, §7).
func (w *studentsWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 16 students operations of BE-023 and BE-025 (04 §2.4); w.svc
// is nil under `koro openapi`, which Register tolerates.
func (w *studentsWiring) routes(c *container, api huma.API) { students.Register(api, w.svc) }

// studentsDSRs adapts kvkk (L13, built after students: read at call time)
// to students.DSRs (03 k11). The request is read as the caller (the dpo:
// kvkk.dsr.read); a request the caller may not see, an unknown one, a
// closed one or one without an account is no request. Without kvkk wired
// the dpo path stays closed.
type studentsDSRs struct{ c *container }

func (d studentsDSRs) OpenSubject(ctx context.Context, a authz.Actor, dsrID uuid.UUID) (uuid.UUID, bool, error) {
	svc := d.c.kvkk.svc
	if svc == nil {
		return uuid.Nil(), false, nil
	}
	r, err := svc.AdminGet(ctx, a, dsrID)
	var pe *problem.Error
	switch {
	case errors.As(err, &pe) && (pe.Status() == http.StatusNotFound || pe.Status() == http.StatusForbidden):
		return uuid.Nil(), false, nil
	case err != nil:
		return uuid.Nil(), false, err
	case r.UserID == nil || !r.Status.Open():
		return uuid.Nil(), false, nil
	}
	return *r.UserID, true, nil
}
