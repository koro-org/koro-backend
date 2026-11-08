package outreach

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
	"uuid"

	"koro/internal/platform/ratelimit"
)

// Count alarms (BE-203): what an org's audience counts raise for review,
// logged at WARN with alarm=true and a count_alarm reason, and counted for
// koro_outreach_count_alarms_total{reason} (cmd/koro wires the metric). An
// alarm never refuses anything by itself.
const (
	// AlarmFamily: an org counted many definitions of one family within 24
	// hours (familyAlarmCounts; families).
	AlarmFamily = "family"
	// AlarmValueFloor: a count was refused by the value floor
	// (outreach.audience_value_too_small).
	AlarmValueFloor = "value_floor"
	// AlarmBudget: a count was refused because the org's daily budget is
	// spent (429 rate_limited).
	AlarmBudget = "budget"
)

// The family watch: an org's fresh counts are counted per definition
// family over 24 hours; up to familyAlarmCounts per family are routine,
// each one beyond raises AlarmFamily. It runs on the count limiter but
// never refuses a count (the daily budget is the limit) and an unavailable
// limiter only skips it.
const (
	familyRuleName    = "outreach_count_family"
	familyAlarmCounts = 30
)

// countAlarms are the count alarm counters of the process (monotonic).
type countAlarms struct{ family, valueFloor, budget atomic.Int64 }

// CountAlarm is one counter of Module.CountAlarms.
type CountAlarm struct {
	Reason string
	Count  int64
}

// CountAlarms returns the count alarm counters of the process (monotonic),
// in a fixed reason order.
func (m Module) CountAlarms() []CountAlarm {
	if m.svc == nil {
		return nil
	}
	a := &m.svc.alarms
	return []CountAlarm{
		{AlarmFamily, a.family.Load()}, {AlarmValueFloor, a.valueFloor.Load()}, {AlarmBudget, a.budget.Load()},
	}
}

// raise counts and logs one alarm of orgID. attrs never hold a definition
// value or a count.
func (s *service) raise(ctx context.Context, reason string, orgID uuid.UUID, attrs ...slog.Attr) {
	switch reason {
	case AlarmFamily:
		s.alarms.family.Add(1)
	case AlarmValueFloor:
		s.alarms.valueFloor.Add(1)
	case AlarmBudget:
		s.alarms.budget.Add(1)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "audience count alarm", append([]slog.Attr{
		slog.Bool("alarm", true), slog.String("count_alarm", reason), slog.String("org_id", orgID.String()),
	}, attrs...)...)
}

// families returns the families of a normalized definition, one per
// present dimension: the dimension's name and a digest of the definition
// without that dimension. Two definitions that differ only inside one
// dimension share that dimension's family.
func families(def Definition) []string {
	var out []string
	for _, name := range dimensionNames {
		if len(def.values(name)) == 0 {
			continue
		}
		sum := sha256.Sum256([]byte(strings.Join(def.without(name).Keys(), "\n")))
		out = append(out, name+":"+hex.EncodeToString(sum[:8]))
	}
	return out
}

// watch counts one fresh count of def by orgID against its families and
// raises AlarmFamily (once per count) when one of them is past
// familyAlarmCounts in 24 hours.
func (s *service) watch(ctx context.Context, orgID uuid.UUID, def Definition) {
	rule := ratelimit.Rule{Name: familyRuleName, Rate: familyAlarmCounts, Per: 24 * time.Hour, Key: ratelimit.KeyOrg}
	hot := ""
	for _, f := range families(def) {
		res, err := s.d.Limiter.Allow(ctx, rule, "org:"+orgID.String()+":"+f)
		if err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "audience count family watch unavailable",
				slog.String("org_id", orgID.String()), slog.String("err", err.Error()))
			return
		}
		if !res.Allowed && hot == "" {
			hot = f
		}
	}
	if hot != "" {
		dim, _, _ := strings.Cut(hot, ":")
		s.raise(ctx, AlarmFamily, orgID, slog.String("dimension", dim))
	}
}
