package talent

import (
	"maps"
	"time"
	"uuid"
)

// The response types of the talent_auditor endpoints (04 §T: "the only
// place a talent value is shown"). They live here, in the talent package,
// and nowhere else: the huma talent field guard (internal/platform/httpx)
// lets talent*, band*, score* and point* members through for the seven
// operations of 04 §T only. A band is an explicit int here, never a Band
// (which refuses every encoder). No DTO carries a name or an e-mail.

// TalentReportDTO is getTalentReport's body.
type TalentReportDTO struct {
	UserID                string                    `json:"user_id" format:"uuid" doc:"The subject (no name or e-mail is ever returned)"`
	Band                  int                       `json:"band" minimum:"0" maximum:"3" doc:"0 neutral (no score, low score, neutralised) .. 3"`
	ScoreCP               *int                      `json:"score_cp" doc:"Raw score of the last nightly run (cp); null before the first run"`
	PctFinalBP            *int                      `json:"pct_final_bp" doc:"Shrunk cohort percentile in basis points; null before the first run"`
	Cohort                *string                   `json:"cohort" doc:"Cohort key of the last run (uni:<id>[:year:<n>] or global)"`
	NSignals              *int                      `json:"n_signals" doc:"Signals of the last run"`
	TopContributors       []TalentContributionDTO   `json:"top_contributors" doc:"Largest decayed contributions of the last run"`
	ConfigVersion         *string                   `json:"config_version" format:"uuid" doc:"Config version of the last run"`
	ComputedAt            *time.Time                `json:"computed_at" doc:"Instant of the last run"`
	CollectionEnabled     bool                      `json:"collection_enabled" doc:"talent.default_enabled"`
	ActivityRankingChoice string                    `json:"activity_ranking_choice" enum:"default,enabled,disabled"`
	Categories            []TalentCategoryDTO       `json:"categories"`
	Signals               []TalentSignalDTO         `json:"signals" doc:"The ledger, oldest first (reversals included)"`
	Neutralizations       []TalentNeutralizationDTO `json:"neutralizations"`
	GeneratedAt           time.Time                 `json:"generated_at"`
}

// TalentContributionDTO is one top contribution.
type TalentContributionDTO struct {
	SignalCode string `json:"signal_code"`
	ValueCP    int    `json:"value_cp"`
}

// TalentCategoryDTO sums the ledger of one category.
type TalentCategoryDTO struct {
	Category string `json:"category"`
	Signals  int    `json:"signals"`
	PointsCP int    `json:"points_cp"`
}

// TalentSignalDTO is one ledger row. point_event_id is what
// reverseTalentPointEvent takes.
type TalentSignalDTO struct {
	PointEventID    string             `json:"point_event_id" format:"uuid"`
	SignalCode      string             `json:"signal_code"`
	Category        string             `json:"category,omitempty" doc:"The signal's category (in the report)"`
	PointsCP        int                `json:"points_cp"`
	RawPointsCP     int                `json:"raw_points_cp"`
	Capped          bool               `json:"capped"`
	CapReason       string             `json:"cap_reason,omitempty" enum:"daily,weekly,per_source"`
	ReasonCode      string             `json:"reason_code"`
	OccurredAt      time.Time          `json:"occurred_at"`
	SourceRef       TalentSourceRefDTO `json:"source_ref"`
	ReversalOf      string             `json:"reversal_of,omitempty" format:"uuid"`
	ConfigVersionID string             `json:"config_version_id" format:"uuid"`
}

// TalentSourceRefDTO names the source of a ledger row (a registration,
// check-in, membership, post, profile section or day; a reversal's source
// is the reversed row).
type TalentSourceRefDTO struct {
	Type string `json:"type"`
	ID   string `json:"id" format:"uuid"`
}

// TalentNeutralizationDTO is one neutralisation (createTalentNeutralization
// and liftTalentNeutralization return it too).
type TalentNeutralizationDTO struct {
	NeutralizationID string     `json:"neutralization_id" format:"uuid"`
	UserID           string     `json:"user_id" format:"uuid"`
	Reason           string     `json:"reason" enum:"opt_out,objection,admin"`
	Note             string     `json:"note,omitempty"`
	DSRID            string     `json:"dsr_id,omitempty" format:"uuid"`
	CreatedBy        string     `json:"created_by,omitempty" format:"uuid"`
	CreatedAt        time.Time  `json:"created_at"`
	Active           bool       `json:"active"`
	LiftedAt         *time.Time `json:"lifted_at,omitempty"`
	LiftedBy         string     `json:"lifted_by,omitempty" format:"uuid"`
}

// TalentDistributionDTO is getTalentDistribution's body.
type TalentDistributionDTO struct {
	UniversityID string                `json:"university_id,omitempty" format:"uuid"`
	Buckets      []TalentBandBucketDTO `json:"buckets"`
}

// TalentBandBucketDTO is the number of users of one band (k ≥ 10).
type TalentBandBucketDTO struct {
	Band           int  `json:"band" minimum:"0" maximum:"3"`
	Count          *int `json:"count" doc:"null below 10"`
	BelowThreshold bool `json:"below_threshold"`
}

// TalentPointEventDTO is reverseTalentPointEvent's body: the reversal row.
type TalentPointEventDTO = TalentSignalDTO

// TalentConfigDTO is getTalentConfig's and createTalentConfigVersion's
// body.
type TalentConfigDTO struct {
	ActiveVersion *TalentConfigVersionDTO `json:"active_version" doc:"null before the first version"`
	SignalDefs    []TalentSignalDefDTO    `json:"signal_defs"`
}

// TalentConfigVersionDTO is one config version.
type TalentConfigVersionDTO struct {
	ConfigVersionID string          `json:"config_version_id" format:"uuid"`
	EffectiveFrom   time.Time       `json:"effective_from"`
	Params          TalentParamsDTO `json:"params"`
	Notes           string          `json:"notes,omitempty"`
	CreatedBy       string          `json:"created_by" format:"uuid"`
	CreatedAt       time.Time       `json:"created_at"`
}

// TalentParamsDTO is the params of a version (02 §15.1). The three gaming
// thresholds are applied by the like consumer and the nightly score
// (BE-085; absent or 0 = off).
type TalentParamsDTO struct {
	ShrinkK                   int                 `json:"shrink_k" doc:"Shrinkage k: w = n / (n + k)"`
	CohortMin                 int                 `json:"cohort_min" doc:"Smallest cohort (≥ 10)"`
	LookbackDays              int                 `json:"lookback_days" doc:"Window of the nightly score (≤ 730)"`
	CategoryCapsCP            map[string]int      `json:"category_caps_cp" doc:"Cap of every category (participation, intent, profile, community, engagement)"`
	Bands                     []TalentBandRuleDTO `json:"bands" doc:"1..3 upward-only bands"`
	MinAccountAgeDaysForLikes *int                `json:"min_account_age_days_for_likes,omitempty" doc:"A like earns points only from an account at least this many days old at the like (absent or 0: off)"`
	MinEventConfirmed         *int                `json:"min_event_confirmed,omitempty" doc:"The nightly score counts the registration, check-in and no-show of an event only with at least this many confirmed registrations (absent or 0: off)"`
	MinEventDistinctCheckins  *int                `json:"min_event_distinct_checkins,omitempty" doc:"The nightly score counts the registration, check-in and no-show of an event only with at least this many distinct users checked in (absent or 0: off)"`
}

// TalentBandRuleDTO is one band rule.
type TalentBandRuleDTO struct {
	Band     int `json:"band" doc:"1, 2 or 3 in order"`
	MinPctBP int `json:"min_pct_bp" doc:"Shrunk percentile that reaches the band (1..10000)"`
	MultBP   int `json:"mult_bp" doc:"Ranking multiplier (10001..11500: at most ×1.15)"`
}

// TalentSignalDefDTO is one signal definition.
type TalentSignalDefDTO struct {
	Code                  string `json:"code"`
	Category              string `json:"category" enum:"participation,intent,profile,community,engagement"`
	BasePointsCP          int    `json:"base_points_cp"`
	DailyCapCP            *int   `json:"daily_cap_cp"`
	WeeklyCapCP           *int   `json:"weekly_cap_cp"`
	HalfLifeDays          *int   `json:"half_life_days" doc:"null: no decay"`
	ExcludedWhenSensitive bool   `json:"excluded_when_sensitive" doc:"Read only: a sensitive source never produces a signal"`
	Enabled               bool   `json:"enabled"`
}

// ---- conversions

func idText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func reportDTO(r Report) TalentReportDTO {
	out := TalentReportDTO{UserID: r.UserID.String(), Band: int(r.Band), CollectionEnabled: r.CollectionEnabled,
		ActivityRankingChoice: string(r.RankingChoice), GeneratedAt: r.GeneratedAt, TopContributors: []TalentContributionDTO{},
		Categories: []TalentCategoryDTO{}, Signals: []TalentSignalDTO{}, Neutralizations: []TalentNeutralizationDTO{}}
	if sc := r.Score; sc != nil {
		raw, pct, n, cohort, v, at := sc.RawCP, sc.PctBP, sc.NSignals, sc.CohortKey, sc.ConfigVersionID.String(), sc.CalculatedAt
		out.ScoreCP, out.PctFinalBP, out.NSignals, out.Cohort, out.ConfigVersion, out.ComputedAt = &raw, &pct, &n, &cohort, &v, &at
		for _, c := range sc.TopContributors {
			out.TopContributors = append(out.TopContributors, TalentContributionDTO{SignalCode: c.Signal, ValueCP: c.ValueCP})
		}
	}
	for _, c := range r.Categories {
		out.Categories = append(out.Categories, TalentCategoryDTO(c))
	}
	for _, p := range r.Signals {
		out.Signals = append(out.Signals, pointDTO(p, r.CategoryOf(p.Signal)))
	}
	for _, n := range r.Neutralizations {
		out.Neutralizations = append(out.Neutralizations, neutralizationDTO(n))
	}
	return out
}

func pointDTO(p PointEvent, category string) TalentSignalDTO {
	return TalentSignalDTO{PointEventID: p.ID.String(), SignalCode: p.Signal, Category: category, PointsCP: p.PointsCP,
		RawPointsCP: p.RawPointsCP, Capped: p.Capped, CapReason: p.CapReason, ReasonCode: p.ReasonCode,
		OccurredAt: p.OccurredAt.UTC(), SourceRef: TalentSourceRefDTO{Type: p.SourceType, ID: p.SourceID.String()},
		ReversalOf: idText(p.ReversalOf), ConfigVersionID: p.ConfigVersionID.String()}
}

func neutralizationDTO(n Neutralization) TalentNeutralizationDTO {
	out := TalentNeutralizationDTO{NeutralizationID: n.ID.String(), UserID: n.UserID.String(), Reason: string(n.Reason),
		Note: n.Note, DSRID: idText(n.DSRID), CreatedBy: idText(n.CreatedBy), CreatedAt: n.CreatedAt.UTC(), Active: n.Active(),
		LiftedBy: idText(n.LiftedBy)}
	if n.LiftedAt != nil {
		at := n.LiftedAt.UTC()
		out.LiftedAt = &at
	}
	return out
}

func distributionDTO(d Distribution) TalentDistributionDTO {
	out := TalentDistributionDTO{UniversityID: idText(d.UniversityID), Buckets: []TalentBandBucketDTO{}}
	for _, b := range d.Buckets {
		out.Buckets = append(out.Buckets, TalentBandBucketDTO{Band: b.Band, Count: b.Count, BelowThreshold: b.Count == nil})
	}
	return out
}

func configDTO(v ConfigView) TalentConfigDTO {
	out := TalentConfigDTO{SignalDefs: []TalentSignalDefDTO{}}
	if a := v.Active; a != nil {
		vd := TalentConfigVersionDTO{ConfigVersionID: a.ID.String(), EffectiveFrom: a.EffectiveFrom.UTC(), Notes: a.Notes,
			CreatedBy: a.CreatedBy.String(), CreatedAt: a.CreatedAt.UTC(), Params: TalentParamsDTO{CategoryCapsCP: map[string]int{},
				Bands: []TalentBandRuleDTO{}}}
		if p, err := ParamsOf(*a); err == nil {
			vd.Params = paramsDTO(p)
		}
		out.ActiveVersion = &vd
	}
	for _, d := range v.Signals {
		// A conversion: a field added to SignalDef stops the build here
		// until the auditor's view decides about it.
		out.SignalDefs = append(out.SignalDefs, TalentSignalDefDTO(d))
	}
	return out
}

func paramsDTO(p ScoreParams) TalentParamsDTO {
	out := TalentParamsDTO{ShrinkK: p.ShrinkK, CohortMin: p.CohortMin, LookbackDays: p.LookbackDays,
		CategoryCapsCP: map[string]int{}, Bands: []TalentBandRuleDTO{}, MinAccountAgeDaysForLikes: p.MinAccountAgeDaysForLikes,
		MinEventConfirmed: p.MinEventConfirmed, MinEventDistinctCheckins: p.MinEventDistinctCheckins}
	maps.Copy(out.CategoryCapsCP, p.CategoryCapsCP)
	for _, b := range p.Bands {
		out.Bands = append(out.Bands, TalentBandRuleDTO(b))
	}
	return out
}

// TalentParamsInputDTO is the params of createTalentConfigVersion. Every
// field is checked by the service after the flag and authorisation (a
// closed API answers 404 whatever the body); the scalars are required.
type TalentParamsInputDTO struct {
	ShrinkK                   *int                `json:"shrink_k,omitempty" doc:"Required. Shrinkage k (0..1000): w = n / (n + k)"`
	CohortMin                 *int                `json:"cohort_min,omitempty" doc:"Required. Smallest cohort (10..1000000)"`
	LookbackDays              *int                `json:"lookback_days,omitempty" doc:"Required. Window of the nightly score (1..730 days)"`
	CategoryCapsCP            map[string]int      `json:"category_caps_cp,omitempty" doc:"Required. A cap (cp) of each of participation, intent, profile, community, engagement"`
	Bands                     []TalentBandRuleDTO `json:"bands,omitempty" doc:"Required. 1..3 rules numbered 1, 2, 3; min_pct_bp and mult_bp grow with the band"`
	MinAccountAgeDaysForLikes *int                `json:"min_account_age_days_for_likes,omitempty" doc:"A like earns points only from an account at least this many days old at the like (absent or 0: off)"`
	MinEventConfirmed         *int                `json:"min_event_confirmed,omitempty" doc:"The nightly score counts the registration, check-in and no-show of an event only with at least this many confirmed registrations (absent or 0: off)"`
	MinEventDistinctCheckins  *int                `json:"min_event_distinct_checkins,omitempty" doc:"The nightly score counts the registration, check-in and no-show of an event only with at least this many distinct users checked in (absent or 0: off)"`
}

// TalentSignalDefInputDTO replaces one signal definition as a whole.
type TalentSignalDefInputDTO struct {
	Code         string `json:"code,omitempty" doc:"Required. An existing signal code"`
	BasePointsCP *int   `json:"base_points_cp,omitempty" doc:"Required (cp; negative for a penalty signal, which never goes below 0 in its category)"`
	DailyCapCP   *int   `json:"daily_cap_cp,omitempty" doc:"Omitted or null: no daily cap"`
	WeeklyCapCP  *int   `json:"weekly_cap_cp,omitempty" doc:"Omitted or null: no weekly cap"`
	HalfLifeDays *int   `json:"half_life_days,omitempty" doc:"Omitted or null: no decay"`
	Enabled      *bool  `json:"enabled,omitempty" doc:"Required"`
}

// missing is the value of an absent required scalar: out of every range,
// so the service's validation reports it.
const missing = -1

func intOr(v *int) int {
	if v == nil {
		return missing
	}
	return *v
}

func paramsOf(d TalentParamsInputDTO) ScoreParams {
	p := ScoreParams{ShrinkK: intOr(d.ShrinkK), CohortMin: intOr(d.CohortMin), LookbackDays: intOr(d.LookbackDays),
		CategoryCapsCP: d.CategoryCapsCP, MinAccountAgeDaysForLikes: d.MinAccountAgeDaysForLikes,
		MinEventConfirmed: d.MinEventConfirmed, MinEventDistinctCheckins: d.MinEventDistinctCheckins}
	for _, b := range d.Bands {
		p.Bands = append(p.Bands, BandRule(b))
	}
	return p
}

func signalUpdatesOf(ds []TalentSignalDefInputDTO) []SignalUpdate {
	out := make([]SignalUpdate, 0, len(ds))
	for _, d := range ds {
		out = append(out, SignalUpdate(d))
	}
	return out
}
