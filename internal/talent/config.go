package talent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"koro/internal/platform/problem"
)

// ScoreParams are the params of a talent.config_versions row (02 §15.1):
// the nightly score's shrinkage, cohort size, look-back window, category
// caps and the upward-only bands. Every value comes from the
// talent_auditor's createTalentConfigVersion (a product decision tuned with
// pilot data, ADR-0015); the code holds no default and the nightly run
// refuses a version that does not validate.
//
// The three gaming thresholds of 02 §15.1 (min_account_age_days_for_likes,
// min_event_confirmed, min_event_distinct_checkins; absent or 0 = off) are
// applied by the like consumer and the nightly score (gaming.go, BE-085)
// with the account age (identity) and the event figures (events).
//
// Signals is the snapshot of the whole signal catalogue the version was
// created with (written by the service, never by the caller): the
// catalogue table changes in place, the version keeps its history.
type ScoreParams struct {
	ShrinkK                   int              `json:"shrink_k"`
	CohortMin                 int              `json:"cohort_min"`
	LookbackDays              int              `json:"lookback_days"`
	CategoryCapsCP            map[string]int   `json:"category_caps_cp"`
	Bands                     []BandRule       `json:"bands"`
	MinAccountAgeDaysForLikes *int             `json:"min_account_age_days_for_likes,omitempty"`
	MinEventConfirmed         *int             `json:"min_event_confirmed,omitempty"`
	MinEventDistinctCheckins  *int             `json:"min_event_distinct_checkins,omitempty"`
	Signals                   []SignalSnapshot `json:"signal_defs,omitempty"`
}

// BandRule lifts a score whose pct_final reaches MinPctBP into Band, whose
// ranking multiplier is MultBP (discovery applies it; 10000 = ×1).
type BandRule struct {
	Band     int `json:"band"`
	MinPctBP int `json:"min_pct_bp"`
	MultBP   int `json:"mult_bp"`
}

// SignalSnapshot is one signal definition inside a config version's params.
type SignalSnapshot struct {
	Code         string `json:"code"`
	Category     string `json:"category"`
	BasePointsCP int    `json:"base_points_cp"`
	DailyCapCP   *int   `json:"daily_cap_cp"`
	WeeklyCapCP  *int   `json:"weekly_cap_cp"`
	HalfLifeDays *int   `json:"half_life_days"`
	Enabled      bool   `json:"enabled"`
}

// SignalUpdate replaces the tunable fields of one signal definition as a
// whole (createTalentConfigVersion signal_defs[]): base points and enabled
// are required; a cap or half-life left out (or null) means none. The code
// and the category are structural (a consumer writes the code; the
// category is capped by the params) and never change; sensitivity always
// excludes a signal (excluded_when_sensitive is not tunable, KVKK md. 6).
type SignalUpdate struct {
	Code         string
	BasePointsCP *int
	DailyCapCP   *int
	WeeklyCapCP  *int
	HalfLifeDays *int
	Enabled      *bool
}

// Bounds of the config validation. They are guards against a broken
// version (a negative window, a ranking effect beyond ADR-0015's ≤ 15 %
// lift, a cohort smaller than the k ≥ 10 rule), not product values.
const (
	maxShrinkK        = 1000
	minCohortMin      = 10 // k ≥ 10: no percentile among fewer people
	maxCohortMin      = 1000000
	maxLookbackDays   = 730 // the ledger is kept 24 months (talent_ledger)
	maxCategoryCapCP  = 1000000
	maxBands          = 3     // band 0 (neutral) .. 3, 02 §15.6
	maxBandMultBP     = 11500 // ADR-0015: the lift is at most ×1.15
	maxGamingParam    = 3650
	maxSignalPointsCP = 100000
	maxSignalCapCP    = 10000000
	maxHalfLifeDays   = 3650
)

var signalCodeRe = regexp.MustCompile(`^[a-z_]{3,40}$`)

// validate returns the field errors of p under location prefix at
// ("body.params").
func (p ScoreParams) validate(at string) []problem.FieldError {
	var fe []problem.FieldError
	bad := func(field, msg string) { fe = append(fe, problem.FieldError{Location: at + "." + field, Message: msg}) }
	if p.ShrinkK < 0 || p.ShrinkK > maxShrinkK {
		bad("shrink_k", fmt.Sprintf("required: 0..%d", maxShrinkK))
	}
	if p.CohortMin < minCohortMin || p.CohortMin > maxCohortMin {
		bad("cohort_min", fmt.Sprintf("required: %d..%d", minCohortMin, maxCohortMin))
	}
	if p.LookbackDays < 1 || p.LookbackDays > maxLookbackDays {
		bad("lookback_days", fmt.Sprintf("required: 1..%d", maxLookbackDays))
	}
	for _, c := range Categories() {
		v, ok := p.CategoryCapsCP[c]
		switch {
		case !ok:
			bad("category_caps_cp."+c, "a cap of every category is required")
		case v < 0 || v > maxCategoryCapCP:
			bad("category_caps_cp."+c, fmt.Sprintf("must be 0..%d", maxCategoryCapCP))
		}
	}
	for c := range p.CategoryCapsCP {
		if !slices.Contains(Categories(), c) {
			bad("category_caps_cp."+c, "unknown category")
		}
	}
	if len(p.Bands) == 0 || len(p.Bands) > maxBands {
		bad("bands", fmt.Sprintf("1..%d bands are required", maxBands))
	}
	for i, b := range p.Bands {
		where := "bands[" + strconv.Itoa(i) + "]"
		if b.Band != i+1 {
			bad(where+".band", "bands are numbered 1, 2, 3 in order (band 0 is neutral)")
		}
		if b.MinPctBP < 1 || b.MinPctBP > 10000 || (i > 0 && b.MinPctBP <= p.Bands[i-1].MinPctBP) {
			bad(where+".min_pct_bp", "must be 1..10000 and grow with the band")
		}
		if b.MultBP <= 10000 || b.MultBP > maxBandMultBP || (i > 0 && b.MultBP <= p.Bands[i-1].MultBP) {
			bad(where+".mult_bp", fmt.Sprintf("must be 10001..%d and grow with the band (upwards only, ADR-0015)", maxBandMultBP))
		}
	}
	for name, v := range map[string]*int{
		"min_account_age_days_for_likes": p.MinAccountAgeDaysForLikes, "min_event_confirmed": p.MinEventConfirmed,
		"min_event_distinct_checkins": p.MinEventDistinctCheckins,
	} {
		if v != nil && (*v < 0 || *v > maxGamingParam) {
			bad(name, fmt.Sprintf("must be 0..%d", maxGamingParam))
		}
	}
	slices.SortFunc(fe, func(a, b problem.FieldError) int { return strings.Compare(a.Location, b.Location) })
	return fe
}

// validateSignalUpdates returns the field errors of the updates under at
// ("body.signal_defs"): known shape, no duplicate code. Whether the code
// exists is checked against the catalogue in the transaction.
func validateSignalUpdates(us []SignalUpdate, at string) []problem.FieldError {
	var fe []problem.FieldError
	seen := map[string]bool{}
	for i, u := range us {
		where := at + "[" + strconv.Itoa(i) + "]"
		bad := func(field, msg string) {
			fe = append(fe, problem.FieldError{Location: where + "." + field, Message: msg})
		}
		switch {
		case !signalCodeRe.MatchString(u.Code):
			bad("code", "must be a signal code")
		case seen[u.Code]:
			bad("code", "listed twice")
		}
		seen[u.Code] = true
		if v := u.BasePointsCP; v == nil || *v < -maxSignalPointsCP || *v > maxSignalPointsCP {
			bad("base_points_cp", fmt.Sprintf("required: -%d..%d", maxSignalPointsCP, maxSignalPointsCP))
		}
		if u.Enabled == nil {
			bad("enabled", "required")
		}
		for name, v := range map[string]*int{"daily_cap_cp": u.DailyCapCP, "weekly_cap_cp": u.WeeklyCapCP} {
			if v != nil && (*v < 1 || *v > maxSignalCapCP) {
				bad(name, fmt.Sprintf("must be null or 1..%d", maxSignalCapCP))
			}
		}
		if v := u.HalfLifeDays; v != nil && (*v < 1 || *v > maxHalfLifeDays) {
			bad("half_life_days", fmt.Sprintf("must be null (no decay) or 1..%d", maxHalfLifeDays))
		}
	}
	return fe
}

// parseParams decodes and validates the params of a stored version (the
// nightly run). A version written before BE-052 (tests) may lack fields:
// the run fails instead of guessing a product value.
func parseParams(raw json.RawMessage) (ScoreParams, error) {
	var p ScoreParams
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := d.Decode(&p); err != nil {
		return ScoreParams{}, fmt.Errorf("%w: %w", errInvalidParams, err)
	}
	if fe := p.validate("params"); len(fe) > 0 {
		return ScoreParams{}, fmt.Errorf("%w: %s %s", errInvalidParams, fe[0].Location, fe[0].Message)
	}
	return p, nil
}

// snapshot is the catalogue entry of a definition.
func snapshot(d SignalDef) SignalSnapshot {
	return SignalSnapshot{Code: d.Code, Category: d.Category, BasePointsCP: d.BasePointsCP, DailyCapCP: d.DailyCapCP,
		WeeklyCapCP: d.WeeklyCapCP, HalfLifeDays: d.HalfLifeDays, Enabled: d.Enabled}
}
