package talent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/students"
)

// subjectReportJob is the system actor of a profiling_access report
// without a first approver.
const subjectReportJob = "talent.subject_report"

// maxReason is audit_events.reason's limit.
const maxReason = 500

// purposeTR states the purpose of the processing in the report (ADR-0015;
// KVKK md. 11/1-c). It is the subject's text, so it is Turkish.
const purposeTR = "Etkinlik ve topluluk aktivitelerine dayalı, yalnız yukarı yönlü bir sıralama bileşeni (talent). " +
	"Hiç kimseye gösterilmez; eşik, eleme veya kara liste olarak kullanılmaz; düşük değer ceza değil nötrdür."

// Report is the person-level talent report (01 §4.11, 03 §5.12): the
// signals with their categories, the band, the nightly score and the
// neutralisations. It leaves the module only through the talent_auditor's
// getTalentReport (BE-052) and a four-eyes approved profiling_access
// export. It carries no name or e-mail.
type Report struct {
	UserID            uuid.UUID
	CollectionEnabled bool
	RankingChoice     students.RankingChoice
	Band              Band
	Scored            bool   // a score row exists (talent.recompute writes them)
	Score             *Score // the score row, nil without one
	Neutralizations   []Neutralization
	Categories        []CategoryTotal
	Signals           []PointEvent
	GeneratedAt       time.Time

	signalCategory map[string]string // signal code → category, for the rendering
}

// CategoryOf returns the category of a signal code of the report.
func (r Report) CategoryOf(signal string) string { return r.signalCategory[signal] }

// CategoryTotal sums the ledger of one category.
type CategoryTotal struct {
	Category string
	Signals  int
	PointsCP int
}

type reportJSON struct {
	UserID            string            `json:"user_id"`
	GeneratedAt       time.Time         `json:"generated_at"`
	Purpose           string            `json:"purpose"`
	CollectionEnabled bool              `json:"collection_enabled"`
	RankingChoice     string            `json:"activity_ranking_choice"`
	Band              int               `json:"band"`
	Scored            bool              `json:"scored"`
	ConfigVersionID   *string           `json:"config_version_id"` // of the score
	CalculatedAt      *time.Time        `json:"calculated_at"`     // of the score
	Neutralizations   []neutralJSON     `json:"neutralizations"`
	Categories        []categoryJSON    `json:"categories"`
	Signals           []reportPointJSON `json:"signals"`
}

type neutralJSON struct {
	Reason    string     `json:"reason"`
	CreatedAt time.Time  `json:"created_at"`
	LiftedAt  *time.Time `json:"lifted_at"`
}

type categoryJSON struct {
	Category string `json:"category"`
	Signals  int    `json:"signals"`
	PointsCP int    `json:"points_cp"`
}

type reportPointJSON struct {
	SignalCode string    `json:"signal_code"`
	Category   string    `json:"category"`
	SourceType string    `json:"source_type"`
	OccurredAt time.Time `json:"occurred_at"`
	PointsCP   int       `json:"points_cp"`
	Capped     bool      `json:"capped"`
	ReasonCode string    `json:"reason_code"`
	Reversal   bool      `json:"reversal"`
}

// JSON renders the report. The band is converted explicitly: this report
// is the one deliberate exception to Band's no-serialisation rule.
func (r Report) JSON() (json.RawMessage, error) {
	out := reportJSON{UserID: r.UserID.String(), GeneratedAt: r.GeneratedAt, Purpose: purposeTR,
		CollectionEnabled: r.CollectionEnabled, RankingChoice: string(r.RankingChoice), Band: int(r.Band), Scored: r.Scored,
		Neutralizations: []neutralJSON{}, Categories: []categoryJSON{}, Signals: []reportPointJSON{}}
	if sc := r.Score; sc != nil {
		v, at := sc.ConfigVersionID.String(), sc.CalculatedAt
		out.ConfigVersionID, out.CalculatedAt = &v, &at
	}
	for _, n := range r.Neutralizations {
		out.Neutralizations = append(out.Neutralizations, neutralJSON{Reason: string(n.Reason), CreatedAt: n.CreatedAt, LiftedAt: n.LiftedAt})
	}
	for _, c := range r.Categories {
		out.Categories = append(out.Categories, categoryJSON(c))
	}
	for _, p := range r.Signals {
		out.Signals = append(out.Signals, reportPointJSON{SignalCode: p.Signal, Category: r.signalCategory[p.Signal], SourceType: p.SourceType,
			OccurredAt: p.OccurredAt, PointsCP: p.PointsCP, Capped: p.Capped, ReasonCode: p.ReasonCode, Reversal: p.ReversalOf != nil})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("talent: report: %w", err)
	}
	return b, nil
}

// SubjectReport implements Auditor: kvkk's profiling_access request, which
// kvkk authorised (four eyes, 03 §5.12). It is reachable only through the
// composition root (hooks.talentAuditor), never from HTTP.
func (s *service) SubjectReport(ctx context.Context, requestedBy, userID uuid.UUID, reason string) (json.RawMessage, error) {
	by := authz.SystemActor(subjectReportJob)
	if requestedBy != uuid.Nil() {
		by = authz.Actor{Kind: authz.KindUser, UserID: requestedBy}
	}
	r, err := s.report(ctx, by, userID, reason)
	if err != nil {
		return nil, err
	}
	return r.JSON()
}

// report reads the subject's talent data and audits the read in the same
// transaction: no report leaves without its audit record.
func (s *service) report(ctx context.Context, by authz.Actor, userID uuid.UUID, reason string) (Report, error) {
	if userID == uuid.Nil() {
		return Report{}, ErrNotFound
	}
	choice, err := s.d.Students.RankingChoice(ctx, userID)
	if err != nil {
		return Report{}, fmt.Errorf("talent: ranking choice: %w", err)
	}
	r := Report{UserID: userID, CollectionEnabled: s.d.CollectionEnabled, RankingChoice: choice, GeneratedAt: s.now(),
		signalCategory: map[string]string{}}
	err = s.inTx(ctx, func(ctx context.Context) error {
		st := s.d.Store
		defs, err := st.SignalDefs(ctx)
		if err != nil {
			return err
		}
		for _, d := range defs {
			r.signalCategory[d.Code] = d.Category
		}
		if r.Signals, err = st.Points(ctx, userID); err != nil {
			return err
		}
		if r.Neutralizations, err = st.Neutralizations(ctx, userID); err != nil {
			return err
		}
		sc, found, err := st.Score(ctx, userID)
		if err != nil {
			return err
		}
		if found {
			r.Score, r.Band, r.Scored = &sc, sc.Band, true
		}
		for _, n := range r.Neutralizations {
			if n.Active() {
				r.Band = BandNeutral // a neutralised user is neutral whatever the stored score says
			}
		}
		r.Categories = categories(r.Signals, r.signalCategory)
		return st.AsApp(ctx, func(ctx context.Context) error {
			uid := userID
			return s.d.Audit.Log(ctx, audit.Event{Action: string(authz.TalentReportRead), Outcome: audit.Success,
				ResourceType: authz.TypeTalentSubject, ResourceID: &uid, Reason: cut(reason, maxReason)}.By(by))
		})
	})
	if err != nil {
		return Report{}, err
	}
	return r, nil
}

// categories sums the ledger by category, in category name order.
func categories(points []PointEvent, catOf map[string]string) []CategoryTotal {
	sums := map[string]*CategoryTotal{}
	for _, p := range points {
		c := catOf[p.Signal]
		t, ok := sums[c]
		if !ok {
			t = &CategoryTotal{Category: c}
			sums[c] = t
		}
		if p.ReversalOf == nil {
			t.Signals++
		}
		t.PointsCP += p.PointsCP
	}
	out := make([]CategoryTotal, 0, len(sums))
	for _, t := range sums {
		out = append(out, *t)
	}
	slices.SortFunc(out, func(a, b CategoryTotal) int { return strings.Compare(a.Category, b.Category) })
	return out
}

// Bands implements Ranking. With collection off every band is neutral and
// the store is not read.
func (s *service) Bands(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]Band, error) {
	if !s.d.CollectionEnabled || len(userIDs) == 0 {
		return map[uuid.UUID]Band{}, nil
	}
	var out map[uuid.UUID]Band
	err := s.inTx(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.d.Store.Bands(ctx, userIDs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
