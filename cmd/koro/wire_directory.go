package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/directory"
	"koro/internal/notify"
	"koro/internal/platform/db"
)

// directoryWiring wires the directory module (L6; 01 §4.8). First owner:
// BE-020; club claims: BE-022. Only tickets of the directory module edit
// this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store     nil → directory.NewGormStore over the app_rw pool
//	newStore  tests: builds the in-memory store over the orgs service (its
//	          listClubs reads the club orgs' names)
//	tx        nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumed: c.orgs.svc (orgs.OrgAdmin.Create in directory's transaction,
// 01 §6.3 T-78; orgs.OrgAdmin.GrantRole of a claim approval, 01 §6.3;
// ActiveMemberIDs/HasRole: the university_rep who approves a claim, E-21;
// public org profiles), c.audit.svc, c.identity.svc (re-authentication of
// adminSetClubSensitivity; claimant names), the identity module's student
// verification (the claimant's university at approval), c.files.svc (claim
// evidence: Get, Attach, PresignGet, Delete — 01 §6.3, BE-057), c.notify.svc
// and c.notify.platformAdmins (the sensitivity request notice and the
// claim decision notice, read at call time). The member count of a club comes from orgs once it offers
// ActiveMemberIDs (BE-019, 01 §4.7); until then member_count is null.
//
// Provided, all set in build: svc (directory.Catalog for later layers), the
// directory KVKK provider (club claims and their evidence),
// hooks.domainResolver (identity .edu.tr resolution, BE-018),
// hooks.clubInfo (orgs joinClub, BE-019) and c.orgs.universityOrgs
// (searchOrgs?university_id, T-180).
type directoryWiring struct {
	store    directory.Store
	newStore func(c *container) directory.Store
	tx       db.TxManager

	mod   directory.Module
	svc   directory.Service // set by build
	built directory.Store   // the store build used (wiring and matrix tests seed it)
}

// activeMembers is the orgs method the member count needs (01 §4.7
// OrgDirectory.ActiveMemberIDs; BE-019).
type activeMembers interface {
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
}

func (w *directoryWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if w.newStore != nil {
		store = w.newStore(c)
	}
	if c.infra.db != nil {
		if store == nil {
			store = directory.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("directory: no store (database not opened)")
	}
	if c.orgs.svc == nil {
		return errors.New("directory: orgs is not built")
	}
	d := directory.Deps{
		Store: store, Tx: tx, Outbox: c.infra.outbox, Queue: c.infra.queue, Audit: c.audit.svc, Orgs: c.orgs.svc,
		Idempotency: c.infra.idempotency, Clock: c.infra.clock, Log: c.infra.log.With("module", "directory"),
		MemberCount: func(ctx context.Context, orgID uuid.UUID) (int, bool, error) {
			am, ok := c.orgs.svc.(activeMembers)
			if !ok {
				return 0, false, nil
			}
			ids, err := am.ActiveMemberIDs(ctx, orgID)
			return len(ids), err == nil, err
		},
		Sender: notifySender{c: c},
		PlatformAdmins: func(ctx context.Context) ([]uuid.UUID, error) {
			if h := c.notify.platformAdmins; h != nil {
				return h(ctx)
			}
			return nil, errors.New("directory: platform admin lookup is not wired")
		},
	}
	if svc := c.identity.svc; svc != nil {
		d.Reauth, d.Users = svc, svc
	}
	if st := c.identity.mod.Students; st != nil {
		d.Students = st
	}
	if fs := c.files.svc; fs != nil {
		d.Files = fs
	}
	mod, err := directory.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.built = mod, mod.Service, store
	c.privacy = append(c.privacy, mod.Privacy)
	c.sweepers = append(c.sweepers, mod.Sweeper)
	svc := mod.Service
	c.hooks.domainResolver = svc.UniversityByEmailDomain
	c.hooks.clubInfo = func(ctx context.Context, clubOrgID uuid.UUID) (hookClub, bool, error) {
		club, err := svc.GetClub(ctx, clubOrgID)
		switch {
		case errors.Is(err, directory.ErrNotFound):
			return hookClub{}, false, nil
		case err != nil:
			return hookClub{}, false, err
		}
		return hookClub{UniversityID: club.UniversityID, JoinPolicy: string(club.JoinPolicy), Sensitive: club.IsSensitive}, true, nil
	}
	c.orgs.universityOrgs = svc.UniversityOrgs
	return nil
}

// tasks: club_claim.decided → directory.notify_claim_decided (01 §6.2), the
// directory.notify_sensitivity_request task (requestClubSensitivity), the
// directory.notify_claim_created task and the daily
// directory.claim_reminders job (06 H-58).
// file.rejected → directory.remove_rejected_evidence is written but not
// subscribed yet (T-222, like identity's).
func (w *directoryWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j) // directory.claim_reminders (06 H-58)
	}
}

// routes: the 22 directory operations (04 §2.3; BE-020's 16, BE-022's 6);
// w.svc is nil under `koro openapi`, which Register tolerates.
func (w *directoryWiring) routes(c *container, api huma.API) { directory.Register(api, w.svc) }

// notifySender adapts notify (L3) to directory.Sender, read at call time;
// a missing sender fails the task (retried), it never drops the notice.
type notifySender struct{ c *container }

func (n notifySender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if n.c.notify.svc == nil {
		return uuid.Nil(), errors.New("directory: notify is not wired")
	}
	return n.c.notify.svc.Send(ctx, m)
}
