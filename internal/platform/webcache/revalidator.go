package webcache

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// EventRef is what the web needs of an event: its public slug and its
// organiser (whose page lists it).
type EventRef struct {
	Slug  string
	OrgID uuid.UUID
}

// OrgRef is what the web needs of an org: its kind (orgs.Kind value) and
// public slug.
type OrgRef struct {
	Kind string
	Slug string
}

// Resolver reads the current slugs (system reads, any status). found is
// false for a missing row; err is retried.
type Resolver interface {
	Event(ctx context.Context, id uuid.UUID) (ref EventRef, found bool, err error)
	Org(ctx context.Context, id uuid.UUID) (ref OrgRef, found bool, err error)
}

// Subscription names (asynq task types, "<module>.<verb>_<object>").
const (
	SubEventPublished   = "webcache.revalidate_published_event"
	SubEventUpdated     = "webcache.revalidate_updated_event"
	SubEventCancelled   = "webcache.revalidate_cancelled_event"
	SubOrgUpdated       = "webcache.revalidate_org"
	SubClubUpdated      = "webcache.revalidate_club"
	SubClubSensitivity  = "webcache.revalidate_club_sensitivity"
	SubClubClaimDecided = "webcache.revalidate_claimed_club"
	SubFilePublished    = "webcache.revalidate_published_file"
	SubFileRejected     = "webcache.revalidate_rejected_file"
	SubPostChanged      = "webcache.revalidate_post"
)

// Publishable file purposes and their owners (files: org_logo attached by
// orgs to the org, event_cover attached by events to the event; BE-120).
const (
	purposeOrgLogo    = "org_logo"
	purposeEventCover = "event_cover"
	ownerOrgs         = "orgs"
	ownerEvents       = "events"
)

// attemptTimeout bounds one task attempt (resolution + POST).
const attemptTimeout = 30 * time.Second

// claimApproved is directory's ClaimApproved outcome: only an approved
// claim changes the public club page (the claimed badge).
const claimApproved = "approved"

// sitemapFields are the event.updated fields that move a page in or out of
// the sitemap (its URL or its public visibility).
var sitemapFields = []string{"slug", "visibility", "visibility_university_id"}

// Deps are the dependencies of the Revalidator.
type Deps struct {
	Resolver Resolver
	// Sender POSTs the tags; nil → the webhook is not configured (dev):
	// every task is skipped with one warning per process.
	Sender *Sender
	Log    *slog.Logger
}

// Revalidator turns public-content events into revalidation requests.
type Revalidator struct {
	d    Deps
	warn sync.Once
}

// New returns the revalidator.
func New(d Deps) *Revalidator { return &Revalidator{d: d} }

// Subscribe registers the webcache.revalidate_* subscriptions (queue
// default, default retries, 30 s per attempt).
func (v *Revalidator) Subscribe(r *outbox.Registry) {
	opts := []outbox.Option{outbox.Queue(queue.Default), outbox.Timeout(attemptTimeout)}
	r.Subscribe(domainevent.TopicEventPublished, SubEventPublished, handle(v, v.eventPublished), opts...)
	r.Subscribe(domainevent.TopicEventUpdated, SubEventUpdated, handle(v, v.eventUpdated), opts...)
	r.Subscribe(domainevent.TopicEventCancelled, SubEventCancelled, handle(v, v.eventCancelled), opts...)
	r.Subscribe(domainevent.TopicOrgUpdated, SubOrgUpdated, handle(v, v.orgUpdated), opts...)
	r.Subscribe(domainevent.TopicClubUpdated, SubClubUpdated, handle(v, v.clubUpdated), opts...)
	r.Subscribe(domainevent.TopicClubSensitivityChanged, SubClubSensitivity, handle(v, v.clubSensitivity), opts...)
	r.Subscribe(domainevent.TopicClubClaimDecided, SubClubClaimDecided, handle(v, v.claimDecided), opts...)
	r.Subscribe(domainevent.TopicFilePublished, SubFilePublished, handle(v, v.filePublished), opts...)
	r.Subscribe(domainevent.TopicFileRejected, SubFileRejected, handle(v, v.fileRejected), opts...)
	r.Subscribe(domainevent.TopicPostChanged, SubPostChanged, handle(v, v.postChanged), opts...)
}

// handle decodes the payload, builds the tags and sends them. A payload
// that does not decode is permanent; resolution and network errors retry.
func handle[P domainevent.Payload](v *Revalidator, tags func(context.Context, P) ([]string, error)) outbox.Handler {
	return func(ctx context.Context, e domainevent.Envelope) error {
		if v.d.Sender == nil {
			v.warn.Do(func() {
				v.d.Log.LogAttrs(ctx, slog.LevelWarn, "web revalidation is not configured: skipped (KORO_WEB_INTERNAL_URL, KORO_REVALIDATE_SECRET)")
			})
			return nil
		}
		p, err := domainevent.Decode[P](e)
		if err != nil {
			return queue.Permanent(err)
		}
		list, err := tags(ctx, p)
		if err != nil {
			return err
		}
		list = v.valid(ctx, e.Topic, list)
		if len(list) == 0 {
			return nil // nothing public to refresh
		}
		if err := v.d.Sender.Send(ctx, list); err != nil {
			return err
		}
		v.d.Log.LogAttrs(ctx, slog.LevelDebug, "web revalidated", slog.String("topic", e.Topic), slog.Int("tags", len(list)))
		return nil
	}
}

// valid drops tags koro-web would reject (the whole request would be 422):
// a slug outside the contract is logged by topic, never by value.
func (v *Revalidator) valid(ctx context.Context, topic string, tags []string) []string {
	return slices.DeleteFunc(dedupe(tags), func(t string) bool {
		if ValidTag(t) {
			return false
		}
		v.d.Log.LogAttrs(ctx, slog.LevelWarn, "web revalidation tag outside the contract: dropped", slog.String("topic", topic))
		return true
	})
}

func (v *Revalidator) eventPublished(ctx context.Context, p domainevent.EventPublished) ([]string, error) {
	return v.eventTags(ctx, p.EventID, p.OrgID, "", true)
}

func (v *Revalidator) eventUpdated(ctx context.Context, p domainevent.EventUpdated) ([]string, error) {
	sitemap := p.PreviousSlug != "" || slices.ContainsFunc(p.Fields, func(f string) bool { return slices.Contains(sitemapFields, f) })
	return v.eventTags(ctx, p.EventID, p.OrgID, p.PreviousSlug, sitemap)
}

func (v *Revalidator) eventCancelled(ctx context.Context, p domainevent.EventCancelled) ([]string, error) {
	return v.eventTags(ctx, p.EventID, p.OrgID, "", true)
}

// eventTags: the event's page (and its old page after a slug change), the
// organiser's page (it lists the events) and optionally the sitemap.
func (v *Revalidator) eventTags(ctx context.Context, eventID, orgID uuid.UUID, previousSlug string, sitemap bool) ([]string, error) {
	var tags []string
	ev, found, err := v.d.Resolver.Event(ctx, eventID)
	if err != nil {
		return nil, fmt.Errorf("webcache: resolve event: %w", err)
	}
	switch {
	case found && ev.Slug != "":
		tags = append(tags, EventTag(ev.Slug))
	case !found && previousSlug == "":
		return nil, nil // no such event: nothing public to refresh
	}
	if previousSlug != "" {
		tags = append(tags, EventTag(previousSlug))
	}
	org, err := v.orgTag(ctx, orgID)
	if err != nil {
		return nil, err
	}
	tags = append(tags, org)
	if sitemap {
		tags = append(tags, TagSitemap)
	}
	return tags, nil
}

func (v *Revalidator) orgUpdated(ctx context.Context, p domainevent.OrgUpdated) ([]string, error) {
	return v.orgTags(ctx, p.OrgID, slices.Contains(p.Fields, "status"))
}

func (v *Revalidator) clubUpdated(ctx context.Context, p domainevent.ClubUpdated) ([]string, error) {
	return v.orgTags(ctx, p.ClubOrgID, slices.Contains(p.Fields, "created"))
}

func (v *Revalidator) clubSensitivity(ctx context.Context, p domainevent.ClubSensitivityChanged) ([]string, error) {
	return v.orgTags(ctx, p.ClubOrgID, true) // the web may hide a sensitive club from the sitemap
}

func (v *Revalidator) claimDecided(ctx context.Context, p domainevent.ClubClaimDecided) ([]string, error) {
	if p.Outcome != claimApproved {
		return nil, nil
	}
	return v.orgTags(ctx, p.ClubOrgID, false)
}

// filePublished: an org logo or event cover got its public media copy
// (BE-120): the owner's page may show it now.
func (v *Revalidator) filePublished(ctx context.Context, p domainevent.FilePublished) ([]string, error) {
	return v.fileOwnerTags(ctx, p.Purpose, p.AttachedModule, p.AttachedEntityID)
}

// fileRejected: an attached org logo or event cover was taken out of use
// (infected, unprocessable): its page must stop showing it.
func (v *Revalidator) fileRejected(ctx context.Context, p domainevent.FileRejected) ([]string, error) {
	if p.AttachedEntityID == nil {
		return nil, nil
	}
	return v.fileOwnerTags(ctx, p.Purpose, p.AttachedModule, *p.AttachedEntityID)
}

// fileOwnerTags: the page of the record that shows a publishable file
// (the org's page; the event's page and its organiser's), nothing for any
// other purpose or owner.
func (v *Revalidator) fileOwnerTags(ctx context.Context, purpose, module string, entityID uuid.UUID) ([]string, error) {
	switch {
	case purpose == purposeOrgLogo && module == ownerOrgs:
		return v.orgTags(ctx, entityID, false)
	case purpose == purposeEventCover && module == ownerEvents:
		ev, found, err := v.d.Resolver.Event(ctx, entityID)
		if err != nil {
			return nil, fmt.Errorf("webcache: resolve event: %w", err)
		}
		if !found {
			return nil, nil
		}
		return v.eventTags(ctx, entityID, ev.OrgID, "", false)
	}
	return nil, nil
}

// postChanged: a post on the public web (Public: an anonymous reader
// opens it) changed: its page and its author org's page (the org page
// lists its posts; university and platform authors have none). Posts are
// not in the sitemap. A post off the public web (followers, segment,
// commercial) has no cached page: nothing is sent.
func (v *Revalidator) postChanged(ctx context.Context, p domainevent.PostChanged) ([]string, error) {
	if !p.Public {
		return nil, nil
	}
	org, err := v.orgTag(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	return []string{PostTag(p.PostID), org}, nil
}

// orgTags: the org's page and optionally the sitemap; an org without a
// public page (university, platform, missing) yields nothing.
func (v *Revalidator) orgTags(ctx context.Context, orgID uuid.UUID, sitemap bool) ([]string, error) {
	tag, err := v.orgTag(ctx, orgID)
	if err != nil || tag == "" {
		return nil, err
	}
	if sitemap {
		return []string{tag, TagSitemap}, nil
	}
	return []string{tag}, nil
}

func (v *Revalidator) orgTag(ctx context.Context, orgID uuid.UUID) (string, error) {
	org, found, err := v.d.Resolver.Org(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("webcache: resolve org: %w", err)
	}
	if !found {
		return "", nil
	}
	return OrgTag(org.Kind, org.Slug), nil
}
