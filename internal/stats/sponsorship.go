package stats

import (
	"context"
	"errors"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/orders"
	"koro/internal/outreach"
)

// companySponsorship is the sponsorship section of a company's panel
// (BE-189): the orders' counts as orders gives them, each event with its
// summary, its organiser and — once held, never for a sensitive event —
// its participation as estimates.
func (s *service) companySponsorship(ctx context.Context, st orders.PurchaseStats) (CompanySponsorshipDTO, error) {
	out := CompanySponsorshipDTO{Orders: st.Orders, ByStatus: keyCounts(st.ByStatus), Lines: st.Lines,
		ByFulfillment: keyCounts(st.ByFulfillment), Delivered: st.ByFulfillment[orders.FulfillFulfilled],
		AwaitingConfirmation: st.ByFulfillment[orders.FulfillMarked], EventCount: st.EventCount,
		Events: make([]SponsoredEventDTO, 0, len(st.Events))}
	if len(st.Events) == 0 {
		return out, nil
	}
	summaries := make(map[uuid.UUID]events.EventSummary, len(st.Events))
	organizers := make([]uuid.UUID, 0, len(st.Events))
	seen := map[uuid.UUID]bool{}
	for _, e := range st.Events {
		if !seen[e.SellerOrgID] {
			seen[e.SellerOrgID] = true
			organizers = append(organizers, e.SellerOrgID)
		}
		if _, ok := summaries[e.EventID]; ok {
			continue
		}
		sum, err := s.d.EventInfo.Get(ctx, e.EventID)
		switch {
		case errors.Is(err, events.ErrNotFound): // kept by the lines' FK; nothing to show but the counts
			continue
		case err != nil:
			return CompanySponsorshipDTO{}, err
		}
		summaries[e.EventID] = sum
	}
	names, err := s.d.Orgs.GetMany(ctx, organizers)
	if err != nil {
		return CompanySponsorshipDTO{}, err
	}
	figures, err := s.sponsoredFigures(ctx, organizers, summaries)
	if err != nil {
		return CompanySponsorshipDTO{}, err
	}
	now := s.d.Clock.Now()
	for _, e := range st.Events {
		org := names[e.SellerOrgID]
		item := SponsoredEventDTO{EventID: e.EventID.String(), OrganizerID: e.SellerOrgID.String(), OrganizerName: org.Name,
			OrganizerKind: string(org.Kind), Orders: e.Orders, OrderStatuses: keyCounts(e.ByStatus), Lines: e.Lines,
			ByFulfillment: keyCounts(e.ByFulfillment), Delivered: e.ByFulfillment[orders.FulfillFulfilled],
			HasEvidence: e.Evidence > 0, EvidenceAttached: e.Evidence, EvidenceRejected: e.EvidenceRejected,
			LastOrderedAt: e.LastPlacedAt.UTC()}
		if sum, ok := summaries[e.EventID]; ok {
			starts := sum.StartsAt.UTC()
			item.Title, item.StartsAt, item.Status = sum.Title, &starts, string(sum.Status)
			item.Held = (sum.Status == events.StatusPublished || sum.Status == events.StatusCompleted) && !sum.StartsAt.After(now)
			if f, ok := figures[e.EventID]; ok && item.Held && !sum.Sensitive {
				item.Participation = &SponsoredParticipationDTO{Registered: estimate(outreach.PublicEstimate(f.Registered)),
					CheckedIn: estimate(outreach.PublicEstimate(f.CheckedIn))}
			}
		}
		out.Events = append(out.Events, item)
	}
	return out, nil
}

// sponsoredFigures reads the participation of the sponsored events held
// by now that are not sensitive, by event: one read over their organisers
// and the span of their starts (an event beyond its MaxOrgEventFigures
// latest is left without a figure).
func (s *service) sponsoredFigures(ctx context.Context, organizers []uuid.UUID, summaries map[uuid.UUID]events.EventSummary) (map[uuid.UUID]events.EventFigures, error) {
	now := s.d.Clock.Now()
	var from, to time.Time
	for _, sum := range summaries {
		if sum.Sensitive || sum.StartsAt.After(now) {
			continue
		}
		if from.IsZero() || sum.StartsAt.Before(from) {
			from = sum.StartsAt
		}
		if sum.StartsAt.After(to) {
			to = sum.StartsAt
		}
	}
	out := map[uuid.UUID]events.EventFigures{}
	if from.IsZero() {
		return out, nil
	}
	figs, err := s.d.Events.OrgEventFigures(ctx, organizers, from, to.Add(time.Second))
	if err != nil {
		return nil, err
	}
	for _, f := range figs {
		if _, ok := summaries[f.EventID]; ok {
			out[f.EventID] = f
		}
	}
	return out, nil
}
