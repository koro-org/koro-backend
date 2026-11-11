package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// The club's side (research §4 "Kulüp profili sahiplenince"): once the
// club's claim is approved its sources stop and its imported events wait
// for its admin as suggestions; promoting one makes an ordinary draft.

// Notice of the new admin (internal/notify/templates/ingest).
const templateSuggestionsReady = "ingest.suggestions_ready"

// PauseClaimedTaskName is the club_claim.decided subscription.
const PauseClaimedTaskName = "ingest.pause_claimed_sources"

// defaultDraftLength is the length of a promoted event without an end.
const defaultDraftLength = 2 * time.Hour

// clubOrg reads the path's org and returns its authorisation resource; an
// org that is not a club is 404.
func (s *service) clubOrg(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, authz.Resource, error) {
	o, err := s.d.Orgs.Get(ctx, orgID)
	switch {
	case errors.Is(err, orgs.ErrNotFound):
		return o, authz.Resource{}, ErrNotFound
	case err != nil:
		return o, authz.Resource{}, err
	}
	if o.Kind != orgs.KindClub {
		return o, authz.Resource{}, ErrNotFound
	}
	return o, authz.OrgResource(o.ID, string(o.Kind), string(o.Status)), nil
}

// ListSuggestions implements Service: the club's upcoming imported events
// (status imported).
func (s *service) ListSuggestions(ctx context.Context, a authz.Actor, orgID uuid.UUID, q EventQuery) (EventPage, error) {
	_, res, err := s.clubOrg(ctx, orgID)
	if err != nil {
		return EventPage{}, err
	}
	if err := authz.Can(ctx, a, authz.IngestSuggestionList, res); err != nil {
		return EventPage{}, err
	}
	q.ClubOrgID = &orgID
	return s.listEvents(ctx, EventFilter{Statuses: []Status{StatusImported}, ClubOrgID: &orgID, From: q.From, To: q.To}, q, true)
}

// suggestion locks one of the club's imported events (another club's or a
// missing one is 404; one no longer "imported" is 409).
func (s *service) suggestion(ctx context.Context, orgID, id uuid.UUID) (ImportedEvent, error) {
	e, err := s.d.Store.LockEvent(ctx, id)
	switch {
	case errors.Is(err, errNotFound):
		return e, ErrNotFound
	case err != nil:
		return e, err
	}
	if e.ClubOrgID == nil || *e.ClubOrgID != orgID {
		return e, ErrNotFound // 06 H-38: the scope comes from the row
	}
	if e.Status != StatusImported {
		return e, ErrInvalidState
	}
	return e, nil
}

// PromoteSuggestion implements Service: in one transaction the club's
// draft event (events.CreateEvent, the actor's own events.event.create
// authorisation) and the imported event marked promoted with the draft's
// ID. The draft carries the facts only (title, time, place text,
// category); the club completes and publishes it.
func (s *service) PromoteSuggestion(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (events.EventView, error) {
	_, res, err := s.clubOrg(ctx, orgID)
	if err != nil {
		return events.EventView{}, err
	}
	if err := authz.Can(ctx, a, authz.IngestSuggestionPromote, res); err != nil {
		return events.EventView{}, err
	}
	if s.d.Drafts == nil {
		return events.EventView{}, errUnavailable
	}
	var out events.EventView
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, err := s.suggestion(ctx, orgID, id)
		if err != nil {
			return err
		}
		v, err := s.d.Drafts.CreateEvent(ctx, a, orgID, draftOf(e))
		if err != nil {
			return err
		}
		draft := v.Event.ID
		if err := s.d.Store.SetEventStatus(ctx, id, StatusPromoted, nil, &draft); err != nil {
			return err
		}
		out = v
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.IngestSuggestionPromote), Outcome: audit.Success, ResourceType: resourceImportedEvent,
			ResourceID: &id, OrgID: &orgID, Metadata: map[string]any{"event_id": draft.String()},
		}.By(a))
	})
	return out, err
}

// draftOf is the draft input of an imported event.
func draftOf(e ImportedEvent) events.EventInput {
	start := e.StartsAt
	end := start.Add(defaultDraftLength)
	switch {
	case e.EndsAt != nil && e.EndsAt.After(start):
		end = *e.EndsAt
	case e.AllDay:
		end = start.Add(24*time.Hour - time.Minute)
	}
	in := events.EventInput{
		Title: clip(e.Title, 160), Category: events.Category(e.Category), Format: events.FormatInPerson,
		StartsAt: start, EndsAt: end, Visibility: events.VisibilityPublic,
	}
	if e.LocationText != nil {
		loc := *e.LocationText
		if strings.EqualFold(strings.TrimSpace(loc), "online") || strings.EqualFold(strings.TrimSpace(loc), "çevrim içi") {
			in.Format = events.FormatOnline
		} else {
			in.LocationName = clip(loc, 200)
		}
	}
	if !in.Category.Valid() {
		in.Category = events.CategoryOther
	}
	return in
}

// RejectSuggestion implements Service: the imported event is declined
// (rejected: off every surface; the row goes at its expiry).
func (s *service) RejectSuggestion(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) error {
	_, res, err := s.clubOrg(ctx, orgID)
	if err != nil {
		return err
	}
	if err := authz.Can(ctx, a, authz.IngestSuggestionReject, res); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.suggestion(ctx, orgID, id); err != nil {
			return err
		}
		if err := s.d.Store.SetEventStatus(ctx, id, StatusRejected, nil, nil); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.IngestSuggestionReject), Outcome: audit.Success, ResourceType: resourceImportedEvent,
			ResourceID: &id, OrgID: &orgID,
		}.By(a))
	})
}

// handleClaimDecided is ingest.pause_claimed_sources (club_claim.decided):
// on an approved claim the club's sources stop (claimed) and, when
// imported events of the club wait, its new admin gets one in-app notice
// (dedupe key: the claim). A rejected or withdrawn claim changes nothing.
func (s *service) handleClaimDecided(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ClubClaimDecided](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.Outcome != "approved" {
		return nil
	}
	n, err := s.d.Store.PauseClubSources(ctx, p.ClubOrgID, PausedClaimed)
	if err != nil {
		return err
	}
	if n > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "ingest sources of a claimed club paused",
			slog.String("club_org_id", p.ClubOrgID.String()), slog.Int("sources", n))
	}
	waiting, err := s.d.Store.CountClubEvents(ctx, p.ClubOrgID, StatusImported, s.now())
	if err != nil || waiting == 0 {
		return err
	}
	if s.d.Sender == nil {
		return errors.New("ingest: notify is not wired")
	}
	to, club := p.UserID, p.ClubOrgID
	_, err = s.d.Sender.Send(ctx, notify.Message{
		UserID: &to, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
		Template: templateSuggestionsReady, SourceOrgID: &club, Data: map[string]any{"club_org_id": club},
		LinkPath: notify.OrgLink(club), DedupeKey: "ingest.suggestions_ready:" + p.ClaimID.String(),
	})
	if err != nil && notify.IsPermanent(err) {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "suggestion notice not deliverable",
			slog.String("claim_id", p.ClaimID.String()), slog.String("err", err.Error()))
		return nil
	}
	if err != nil {
		return fmt.Errorf("ingest: suggestion notice: %w", err)
	}
	return nil
}
