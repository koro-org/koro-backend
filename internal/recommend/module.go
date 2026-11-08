// Package recommend is the rule-based recommendations of a student
// (BE-174; uyum denetimi #25 "kural bazlı öneri"): upcoming events, clubs
// and open opportunity calls (BE-191) picked by a handful of explainable
// rules over the student's own data, each item with the reason codes of
// the rules that picked it. No machine learning, no score in the answer,
// nothing stored: the module owns no table, writes nothing and computes
// the lists on every request from small read ports (ports.go) that the
// composition root fills with the system reads of the modules that own the
// rows (students, events, directory, feed, messaging). It sits in L13 next to stats
// (01 §5.1): nothing imports it.
//
// Rules the package enforces:
//
//   - Who (03 §5.24): the caller's own recommendations, a verified
//     student's (student, student_minor); every other signed-in user 403,
//     another user's 404. Nobody reads another student's recommendations,
//     platform staff included.
//   - Own data, for the student only (KVKK md. 5/2-c, f; no profiling for
//     a third party): the signals are the student's follows and
//     memberships, the categories and organisers of the events the student
//     held a place in, the student's university and class year; the only
//     data about other people is a popularity flag over at least 10 of
//     them (k ≥ 10: an event's held places, the eligible members of a club
//     in the student's department or university, an opportunity call's
//     likes or saves), counted where the rows live; no count leaves.
//     Nothing is shown to or shared with an org, a company or an
//     institution; nothing is logged or audited beyond the request line.
//   - Already in touch (BE-198): an opportunity call a company contacted
//     the student about (a contact request naming it, whatever its status)
//     is not recommended, like a call the student liked or saved; the read
//     is the student's own requests (call IDs only), and its error answers
//     5xx like every other read.
//   - Special-category data (03 Y2, KVKK md. 6): a sensitive club (health,
//     religion, political opinion …) or an event of a sensitive category is
//     never a signal and never a recommendation, whatever rule named it; an
//     opportunity call of a sensitive club or addressed to its members
//     neither; an error reading the sensitivity answers 5xx (fail closed).
//   - Visibility and targeting (03 §3.6): an event or an opportunity call
//     is recommended only to a student who may see it (the audience, a
//     followers post for a follower or member, no commercial post for a
//     minor, not muted or hidden).
//   - Opt-out (KVKK md. 11): the student turns the recommendations off in
//     the privacy settings (students recommendation_choice, updateMyPrivacy).
//     "default" is on for an adult and off for a minor (Enabled); a student
//     whose recommendations are off gets enabled=false and empty lists, and
//     no signal is read.
//   - Bounded: one request reads a fixed number of ports (two event store
//     reads and two opportunity store reads of at most Candidates rows
//     each, one follow read of at most MaxFollows IDs, one contact request
//     read of at most feed.MaxExcludedOpportunities call IDs, two popularity
//     counts, one club card read and one call card read of at most
//     MaxLimit cards); no cache (the lists change with every registration,
//     like and save).
package recommend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"koro/internal/authz"
	"koro/internal/platform/clock"
)

// Service is the recommend service. Every method calls authz.Can first
// (05 §5.3).
type Service interface {
	// List is listMyRecommendations (recommend.recommendation.list_own).
	List(ctx context.Context, a authz.Actor, q Query) (Recommendations, error)
}

// Deps are the dependencies of the recommend module; all are required.
type Deps struct {
	Students      Students
	Events        Events
	Clubs         Clubs
	Follows       Follows
	Opportunities Opportunities
	// ContactRequests leaves out the calls the student was contacted about
	// (BE-198).
	ContactRequests ContactRequests
	Clock           clock.Clock
	Log             *slog.Logger
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Students", d.Students != nil}, {"Events", d.Events != nil}, {"Clubs", d.Clubs != nil},
		{"Follows", d.Follows != nil}, {"Opportunities", d.Opportunities != nil},
		{"ContactRequests", d.ContactRequests != nil}, {"Clock", d.Clock != nil},
		{"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("recommend.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what recommend hands to the composition root.
type Module struct {
	Service Service
}

// New builds the recommend module; a missing dependency is a wiring bug
// that stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	return Module{Service: &service{d: d}}, nil
}

type service struct {
	d Deps
}
