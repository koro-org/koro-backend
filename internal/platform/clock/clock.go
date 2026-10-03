// Package clock is the only place that reads the wall clock (05 §9.2).
// Services, handlers, stores and jobs receive a Clock and never call time.Now.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time in UTC.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Real returns the system clock.
func Real() Clock { return realClock{} }

// Istanbul returns the Europe/Istanbul location used at presentation
// boundaries (e-mails, ICS, exports, "today" calculations). The binary embeds
// time/tzdata (cmd/koro), so loading cannot fail; if it does, the process
// panics at first use.
func Istanbul() *time.Location { return istanbul() }

var istanbul = sync.OnceValue(func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		panic("clock: load Europe/Istanbul: " + err.Error())
	}
	return loc
})

// Fake is a manually advanced clock for tests. It is safe for concurrent use.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a fake clock set to t (converted to UTC).
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake time forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set moves the fake time to t (converted to UTC).
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}
