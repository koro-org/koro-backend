package recommend

import (
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/feed"
)

// RecommendationsDTO is listMyRecommendations (04 §2.24): the student's own
// recommendations, never shown to anyone else. No score, rank or count: an
// item carries the codes of the rules that picked it.
type RecommendationsDTO struct {
	Enabled bool                  `json:"enabled" doc:"Whether your recommendations are on (choice; default: on for an adult, off for a minor). Off: both lists are empty and nothing of yours was read"`
	Choice  string                `json:"choice" enum:"default,enabled,disabled" doc:"Your recommendation_choice of the privacy settings (getMyPrivacy, updateMyPrivacy)"`
	Events  []RecommendedEventDTO `json:"events" doc:"Upcoming events (next 30 days) you have not registered for, the best match first"`
	Clubs   []RecommendedClubDTO  `json:"clubs" doc:"Clubs you neither follow nor belong to, the best match first"`
	// Opportunities: BE-191.
	Opportunities []RecommendedOpportunityDTO `json:"opportunities" doc:"Open opportunity calls (published in the last 60 days, deadline not over) you see in your feed and have neither liked nor saved, the best match first"`
}

// RecommendedEventDTO is a recommended event: listEvents' card and why.
type RecommendedEventDTO struct {
	Event   events.EventCardDTO `json:"event"`
	Reasons []string            `json:"reasons" minItems:"1" enum:"followed_org,matches_interest,attended_category,same_university,popular,starting_soon" doc:"The rules that picked the event, the strongest first (show the first; starting_soon never comes alone): followed_org — a club or org you follow or belong to; matches_interest — its category matches one of your profile's interests; attended_category — like the events you attended in the last year; same_university — your university or one of its clubs; popular — at least 10 registrations; starting_soon — starts within 72 hours"`
}

// RecommendedClubDTO is a recommended club: listClubs' card and why.
type RecommendedClubDTO struct {
	Club    directory.ClubCardDTO `json:"club"`
	Reasons []string              `json:"reasons" minItems:"1" enum:"attended_organiser,matches_interest,popular_in_department,popular_in_university" doc:"The rules that picked the club, the strongest first (show the first): attended_organiser — it organised an event you attended; matches_interest — its category is one of your profile's interests (a club of your university, or one another rule picked); popular_in_department — at least 10 students of your department are members; popular_in_university — at least 10 students of your university are members"`
}

// RecommendedOpportunityDTO is a recommended opportunity call: getFeed's
// post and why (BE-191).
type RecommendedOpportunityDTO struct {
	Post    feed.PostDTO `json:"post"`
	Reasons []string     `json:"reasons" minItems:"1" enum:"followed_org,attended_organiser,matches_interest,same_university,eligible_year,popular,deadline_soon" doc:"The rules that picked the call, the strongest first (show the first; deadline_soon never comes alone): followed_org — a club or org you follow or belong to; attended_organiser — it organised an event you attended; matches_interest — its type matches one of your profile's interests; same_university — your university or one of its clubs; eligible_year — open to your class year; popular — at least 10 likes or 10 saves; deadline_soon — the deadline is within 7 days"`
}

func toDTO(r Recommendations) RecommendationsDTO {
	out := RecommendationsDTO{
		Enabled: r.Enabled, Choice: string(r.Choice),
		Events: make([]RecommendedEventDTO, len(r.Events)), Clubs: make([]RecommendedClubDTO, len(r.Clubs)),
		Opportunities: make([]RecommendedOpportunityDTO, len(r.Opportunities)),
	}
	for i, it := range r.Opportunities {
		out.Opportunities[i] = RecommendedOpportunityDTO{Post: feed.PostDTOOf(it.View), Reasons: codes(it.Reasons)}
	}
	for i, it := range r.Events {
		out.Events[i] = RecommendedEventDTO{Event: events.CardOf(it.View), Reasons: codes(it.Reasons)}
	}
	for i, it := range r.Clubs {
		out.Clubs[i] = RecommendedClubDTO{Club: directory.ClubCardOf(it.Card), Reasons: codes(it.Reasons)}
	}
	return out
}

func codes(rs []Reason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}
