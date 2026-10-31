package stats

import (
	"time"

	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/problem"
)

// RangeQuery is the from and to query parameters of a panel as received
// ("" = the panel's default).
type RangeQuery struct {
	From string
	To   string
}

// MaxRangeDays bounds a panel's period (the reads stay on their indexes).
const MaxRangeDays = 366

// period is a resolved range: whole Istanbul days [from, to].
type period struct {
	from, to civil.Date
}

// defaultFrom picks a panel's first day for its last day.
type defaultFrom func(to civil.Date) civil.Date

// lastDays is the default of the platform and company panels: the last 30
// days, today included.
func lastDays(to civil.Date) civil.Date { return to.AddDays(-29) }

// lastMonths is the default of the club and university panels: the 12
// calendar months that end with the month of to (monthly growth).
func lastMonths(to civil.Date) civil.Date {
	return civil.From(time.Date(to.Year, to.Month-11, 1, 0, 0, 0, 0, time.UTC))
}

// resolve turns q into a period at now: 422 validation_failed for a
// malformed date, from after to or a range longer than MaxRangeDays.
func (q RangeQuery) resolve(now time.Time, def defaultFrom) (period, error) {
	to := civil.Today(now)
	if q.To != "" {
		d, err := civil.Parse(q.To)
		if err != nil {
			return period{}, problem.Field("query.to", "must be a date (YYYY-MM-DD)")
		}
		to = d
	}
	from := def(to)
	if q.From != "" {
		d, err := civil.Parse(q.From)
		if err != nil {
			return period{}, problem.Field("query.from", "must be a date (YYYY-MM-DD)")
		}
		from = d
	}
	switch {
	case from.After(to):
		return period{}, problem.Field("query.from", "must not be after to")
	case from.AddDays(MaxRangeDays - 1).Before(to):
		return period{}, problem.Field("query.from", "the range is at most 366 days")
	}
	return period{from: from, to: to}, nil
}

// bounds is the period as instants: [first day 00:00, day after the last
// 00:00) in Europe/Istanbul.
func (p period) bounds() (time.Time, time.Time) {
	loc := clock.Istanbul()
	return p.from.In(loc).UTC(), p.to.AddDays(1).In(loc).UTC()
}

// dto is the period as answered.
func (p period) dto() RangeDTO { return RangeDTO{From: p.from.String(), To: p.to.String()} }

// months lists the "YYYY-MM" months of the period, oldest first.
func (p period) months() []string {
	var out []string
	for m := time.Date(p.from.Year, p.from.Month, 1, 0, 0, 0, 0, time.UTC); !civil.From(m).After(p.to); m = m.AddDate(0, 1, 0) {
		out = append(out, m.Format("2006-01"))
	}
	return out
}
