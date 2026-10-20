package talent

import (
	"slices"
	"strings"
	"sync"
)

// Outcomes of a consumed signal: the outcome label of
// koro_talent_ingest_total{signal, outcome}. Both labels come from fixed
// sets (signal codes, these constants); the metric carries no ID and no
// talent value, only how many events took each path.
const (
	OutcomeWritten        = "written"         // a ledger row with its full points
	OutcomeCapped         = "capped"          // a ledger row cut by a daily, weekly or per-source cap
	OutcomeReversed       = "reversed"        // a reversal row
	OutcomeDuplicate      = "duplicate"       // the event was consumed before (dedupe key)
	OutcomeFlagOff        = "flag_off"        // talent.default_enabled is false (T-32): nothing written
	OutcomeSensitive      = "sensitive"       // sensitive event or club (KVKK md. 6)
	OutcomeNotStudent     = "not_student"     // no student profile
	OutcomeMinor          = "minor"           // under 18 (T-22, T-23)
	OutcomeOptedOut       = "opted_out"       // activity_ranking_choice = disabled
	OutcomeNeutralized    = "neutralized"     // an active opt-out or objection neutralisation
	OutcomeSignalDisabled = "signal_disabled" // signal_defs.enabled = false or no such signal
	OutcomeNoConfig       = "no_config"       // no config version in force (BE-052 creates them)
	OutcomeNoOriginal     = "no_original"     // a reversal without its original row
)

// CounterSample is one value of koro_talent_ingest_total.
type CounterSample struct {
	Signal, Outcome string
	Value           uint64
}

// counters are the in-process totals behind koro_talent_ingest_total
// (read at scrape time; monotonic for the life of the process).
type counters struct {
	mu sync.Mutex
	m  map[[2]string]uint64
}

func newCounters() *counters { return &counters{m: map[[2]string]uint64{}} }

func (c *counters) inc(signal, outcome string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[[2]string{signal, outcome}]++
}

func (c *counters) snapshot() []CounterSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CounterSample, 0, len(c.m))
	for k, v := range c.m {
		out = append(out, CounterSample{Signal: k[0], Outcome: k[1], Value: v})
	}
	slices.SortFunc(out, func(a, b CounterSample) int {
		if c := strings.Compare(a.Signal, b.Signal); c != 0 {
			return c
		}
		return strings.Compare(a.Outcome, b.Outcome)
	})
	return out
}

// Counters returns koro_talent_ingest_total by signal and outcome (the
// worker registers it on its metrics registry).
func (m Module) Counters() []CounterSample { return m.svc.counts.snapshot() }
