package audittest

import (
	"context"
	"slices"
	"sync"

	"koro/internal/audit"
	"koro/internal/authz"
)

// Recorder is the in-memory audit.Logger of module service tests: it keeps
// every logged event in order. Fail and FailOn make Log return an error,
// for the tests of an operation whose audit write fails (the business
// change rolls back with it). The zero value is ready to use; it is safe
// for concurrent use.
type Recorder struct {
	mu       sync.Mutex
	events   []audit.Event
	fail     error
	failOn   string
	failOnTo error
}

var _ audit.Logger = (*Recorder)(nil)

// Log implements audit.Logger.
func (r *Recorder) Log(_ context.Context, e audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	if r.failOnTo != nil && e.Action == r.failOn {
		return r.failOnTo
	}
	r.events = append(r.events, e)
	return nil
}

// Fail makes every later Log return err (nil: log again).
func (r *Recorder) Fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = err
}

// FailOn makes every later Log of action return err (nil err: log again).
func (r *Recorder) FailOn(action authz.Action, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOn, r.failOnTo = string(action), err
}

// All returns a copy of the logged events.
func (r *Recorder) All() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// Of returns the logged events of action.
func (r *Recorder) Of(action authz.Action) []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []audit.Event
	for _, e := range r.events {
		if e.Action == string(action) {
			out = append(out, e)
		}
	}
	return out
}

// OfOutcome returns the logged events of action with outcome o.
func (r *Recorder) OfOutcome(action authz.Action, o audit.Outcome) []audit.Event {
	var out []audit.Event
	for _, e := range r.Of(action) {
		if e.Outcome == o {
			out = append(out, e)
		}
	}
	return out
}

// Actions returns the actions of the logged events in order.
func (r *Recorder) Actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.Action
	}
	return out
}

// Last returns the last logged event (the zero Event when none).
func (r *Recorder) Last() audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return audit.Event{}
	}
	return r.events[len(r.events)-1]
}

// Reset forgets the logged events.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

// RowOf is e in the terms of the reauth checks (ReauthCase.Denied).
func RowOf(e audit.Event) Row {
	return Row{Action: e.Action, Outcome: string(e.Outcome), Reason: e.Reason, ActorUserID: e.ActorUserID,
		ActorOrgID: e.ActorOrgID, SystemJob: e.SystemJob, OrgID: e.OrgID, ResourceType: e.ResourceType, ResourceID: e.ResourceID,
		Metadata: e.Metadata}
}

// Rows returns the logged events of action as Rows (ReauthCase.Rows).
func (r *Recorder) Rows(action authz.Action) []Row {
	var out []Row
	for _, e := range r.Of(action) {
		out = append(out, RowOf(e))
	}
	return out
}
