package recommend

import (
	"koro/internal/directory"
	"koro/internal/events"
)

// RecommendationsDTO is listMyRecommendations (04 §2.24): the student's own
// recommendations, never shown to anyone else. No score, rank or count: an
// item carries the codes of the rules that picked it.
type RecommendationsDTO struct {
	Enabled bool                  `json:"enabled" doc:"Whether your recommendations are on (choice; default: on for an adult, off for a minor). Off: both lists are empty and nothing of yours was read"`
	Choice  string                `json:"choice" enum:"default,enabled,disabled" doc:"Your recommendation_choice of the privacy settings (getMyPrivacy, updateMyPrivacy)"`
	Events  []RecommendedEventDTO `json:"events" doc:"Upcoming events (next 30 days) you have not registered for, the best match first"`
	Clubs   []RecommendedClubDTO  `json:"clubs" doc:"Clubs you neither follow nor belong to, the best match first"`
}

// RecommendedEventDTO is a recommended event: listEvents' card and why.
type RecommendedEventDTO struct {
	Event   events.EventCardDTO `json:"event"`
	Reasons []string            `json:"reasons" minItems:"1" enum:"followed_org,attended_category,same_university,popular,starting_soon" doc:"The rules that picked the event, the strongest first (show the first; starting_soon never comes alone): followed_org — a club or org you follow or belong to; attended_category — like the events you attended in the last year; same_university — your university or one of its clubs; popular — at least 10 registrations; starting_soon — starts within 72 hours"`
}

// RecommendedClubDTO is a recommended club: listClubs' card and why.
type RecommendedClubDTO struct {
	Club    directory.ClubCardDTO `json:"club"`
	Reasons []string              `json:"reasons" minItems:"1" enum:"attended_organiser,popular_in_department,popular_in_university" doc:"The rules that picked the club, the strongest first (show the first): attended_organiser — it organised an event you attended; popular_in_department — at least 10 students of your department are members; popular_in_university — at least 10 students of your university are members"`
}

func toDTO(r Recommendations) RecommendationsDTO {
	out := RecommendationsDTO{
		Enabled: r.Enabled, Choice: string(r.Choice),
		Events: make([]RecommendedEventDTO, len(r.Events)), Clubs: make([]RecommendedClubDTO, len(r.Clubs)),
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
