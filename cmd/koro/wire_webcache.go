package main

import (
	"context"
	"errors"
	"net/http"
	"uuid"

	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/webcache"
)

// webRevalidation composes the publication webhook (BE-106,
// internal/platform/webcache): the webcache.revalidate_* subscriptions on
// event.published / event.updated / event.cancelled (events), org.updated
// (orgs), club.updated / club.sensitivity_changed / club_claim.decided
// (directory), file.published / file.rejected (files: org logos and event
// covers, BE-120), post.changed (feed: public posts, BE-149). It is platform, not a feature module: wire.go registers it
// with the platform's tasks (c.web.tasks), after every module is built,
// because the resolver reads events and orgs.
//
// Configuration: config.Worker.Webcache (c.infra.webcache), the worker's
// alone. Outside `koro worker` (api, seed, wiring tests) it is empty: the
// subscriptions exist (one registry everywhere) but skip with a warning.
//
// Seams: client (tests preset an HTTP client), rev (set by tasks; tests
// read it).
type webRevalidation struct {
	client *http.Client
	rev    *webcache.Revalidator
}

func (w *webRevalidation) tasks(c *container) {
	cfg := c.infra.webcache
	var sender *webcache.Sender
	if cfg.Enabled() {
		s, err := webcache.NewSender(webcache.SenderConfig{
			BaseURL: cfg.WebInternalURL, Secret: cfg.RevalidateSecret.Reveal(), Clock: c.infra.clock, Client: w.client,
		})
		if err != nil {
			c.infra.log.Error("web revalidation not wired", "err", err.Error())
		}
		sender = s
	}
	w.rev = webcache.New(webcache.Deps{
		Resolver: webResolver{c: c}, Sender: sender, Log: c.infra.log.With("component", "webcache"),
	})
	w.rev.Subscribe(c.infra.subs)
}

// webResolver reads the current slugs through the modules' system reads
// (events.Events.Get, orgs.OrgDirectory.Get; any status).
type webResolver struct{ c *container }

func (r webResolver) Event(ctx context.Context, id uuid.UUID) (webcache.EventRef, bool, error) {
	svc := r.c.events.svc
	if svc == nil {
		return webcache.EventRef{}, false, errors.New("webcache: events is not wired")
	}
	e, err := svc.Get(ctx, id)
	switch {
	case errors.Is(err, problem.ErrNotFound), errors.Is(err, events.ErrNotFound):
		return webcache.EventRef{}, false, nil
	case err != nil:
		return webcache.EventRef{}, false, err
	}
	return webcache.EventRef{Slug: e.Slug, OrgID: e.OrgID}, true, nil
}

func (r webResolver) Org(ctx context.Context, id uuid.UUID) (webcache.OrgRef, bool, error) {
	svc := r.c.orgs.svc
	if svc == nil {
		return webcache.OrgRef{}, false, errors.New("webcache: orgs is not wired")
	}
	o, err := svc.Get(ctx, id)
	switch {
	case errors.Is(err, problem.ErrNotFound), errors.Is(err, orgs.ErrNotFound):
		return webcache.OrgRef{}, false, nil
	case err != nil:
		return webcache.OrgRef{}, false, err
	}
	return webcache.OrgRef{Kind: string(o.Kind), Slug: o.Slug}, true, nil
}
