package httpx_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

type guardItem struct {
	Name        string `json:"name"`
	TalentScore int    `json:"talent_score"`
}

type guardNested struct {
	Body struct {
		Items []guardItem `json:"items"`
	}
}

type guardBand struct {
	Body struct {
		UserID string `json:"user_id"`
		Band   int    `json:"band"`
	}
}

type guardPoints struct {
	Points int `query:"points"`
}

type guardMap struct {
	Body struct {
		ByUser map[string]struct {
			PointsCP int `json:"points_cp"`
		} `json:"by_user"`
	}
}

type guardRequest struct {
	Body struct {
		ScoreMin int `json:"score_min"`
	}
}

type guardAllowed struct {
	Body struct {
		SizeBand              string `json:"size_band"`
		ActivityRankingChoice string `json:"activity_ranking_choice"`
		Bandwidth             int    `json:"bandwidth"`
	}
}

// SuggestionDTO mirrors payments' match suggestion before BE-081 renamed
// score to match_confidence: there is no exemption, so score is rejected.
type SuggestionDTO struct {
	Score float64 `json:"score"`
}

type guardSuggestion struct {
	Body struct {
		Suggestions []SuggestionDTO `json:"suggestions"`
	}
}

type guardEmpty struct{}

func guardApp(t *testing.T) huma.API {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	return app.V1
}

func guardOp(id, tag string) huma.Operation {
	return huma.Operation{OperationID: id, Method: http.MethodGet, Path: "/" + id, Tags: []string{tag}}
}

// register registers an operation and returns the panic message ("" when
// it registered).
func register[I, O any](api huma.API, o huma.Operation) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	huma.Register(api, o, func(context.Context, *I) (*O, error) { return nil, nil })
	return ""
}

// TestTalentFieldGuard_RejectsTalentFields (BE-047; 01 §4.11, 04 §T,
// §V.0-2): an operation outside the talent module cannot expose a talent*,
// band, score* or point* member anywhere in its request or response (nested
// objects, arrays, maps) or as a parameter; registration panics.
func TestTalentFieldGuard_RejectsTalentFields(t *testing.T) {
	t.Parallel()
	for name, reg := range map[string]func(api huma.API) string{
		"nested array member": func(api huma.API) string {
			return register[guardEmpty, guardNested](api, guardOp("nested", "students"))
		},
		"band":             func(api huma.API) string { return register[guardEmpty, guardBand](api, guardOp("band", "discovery")) },
		"query parameter":  func(api huma.API) string { return register[guardPoints, guardEmpty](api, guardOp("points", "feed")) },
		"map value member": func(api huma.API) string { return register[guardEmpty, guardMap](api, guardOp("bymap", "events")) },
		"no exemption (SuggestionDTO.score, BE-081)": func(api huma.API) string {
			return register[guardEmpty, guardSuggestion](api, guardOp("suggest", "payments"))
		},
		"request body": func(api huma.API) string {
			o := guardOp("filter", "messaging")
			o.Method = http.MethodPost
			return register[guardRequest, guardEmpty](api, o)
		},
	} {
		msg := reg(guardApp(t))
		if msg == "" || !strings.Contains(msg, "talent field") {
			t.Errorf("%s: registration did not panic with a talent field message (%q)", name, msg)
		}
	}
}

// TestTalentFieldGuard_AllowsTalentOperations: the talent_auditor
// operations of 04 §T (BE-052) carry the band; size_band, bandwidth and
// activity_ranking_choice pass.
func TestTalentFieldGuard_AllowsTalentOperations(t *testing.T) {
	t.Parallel()
	api := guardApp(t)
	if msg := register[guardEmpty, guardBand](api, auditorOp("getTalentReport", http.MethodGet, "/admin/talent/subjects/{user_id}/report")); msg != "" {
		t.Fatalf("talent_auditor operation rejected: %s", msg)
	}
	if msg := register[guardPointPath, guardEmpty](api,
		auditorOp("reverseTalentPointEvent", http.MethodPost, "/admin/talent/point-events/{point_event_id}/reversal")); msg != "" {
		t.Fatalf("talent_auditor point_event_id parameter rejected: %s", msg)
	}
	if msg := register[guardEmpty, guardAllowed](api, guardOp("allowed", "orgs")); msg != "" {
		t.Fatalf("allowed members rejected: %s", msg)
	}
	if v := httpx.TalentFieldViolations(api.OpenAPI()); len(v) != 0 {
		t.Fatalf("violations = %q, want none", v)
	}
	// A document assembled without the hook (written into Paths directly)
	// is still caught by the whole-document check.
	oapi := api.OpenAPI()
	oapi.Paths["/sneaky"] = &huma.PathItem{Get: &huma.Operation{OperationID: "sneaky", Tags: []string{"students"},
		Parameters: []*huma.Param{{Name: "talent", In: "query", Schema: &huma.Schema{Type: "string"}}}}}
	if v := httpx.TalentFieldViolations(oapi); len(v) != 1 || !strings.Contains(v[0], `parameter "talent"`) {
		t.Fatalf("violations = %q, want the sneaky parameter", v)
	}
}

type guardPointPath struct {
	PointEventID string `path:"point_event_id"`
}

// TalentReportDTO mirrors the talent_auditor report (a talent DTO another
// operation must not reuse).
type TalentReportDTO struct {
	UserID string `json:"user_id"`
	Band   int    `json:"band"`
}

type guardReport struct{ Body TalentReportDTO }

// auditorOp is a talent_auditor operation as op.Koro writes it (04 §T).
func auditorOp(id, method, path string) huma.Operation {
	return huma.Operation{OperationID: id, Method: method, Path: path, Tags: []string{"talent"},
		Extensions: map[string]any{"x-koro-action": "talent.report.read", "x-koro-surface": "admin"}}
}

// TestTalentFieldGuard_AuditorExemptionIsNarrow (BE-052): the exemption is
// the seven operations of 04 §T exactly — operationId, method and path,
// the talent tag, a talent.* action on the admin surface. Every other
// combination is checked like any operation and registration panics: the
// talent tag alone, a listed operationId on another path, method, tag,
// action or surface, and an operation of another module that reuses a
// talent DTO registered by an exempt operation.
func TestTalentFieldGuard_AuditorExemptionIsNarrow(t *testing.T) {
	t.Parallel()
	const reportPath = "/admin/talent/subjects/{user_id}/report"
	for name, o := range map[string]huma.Operation{
		"talent tag, unlisted operation":   auditorOp("getTalentScores", http.MethodGet, "/admin/talent/scores"),
		"listed operation on another path": auditorOp("getTalentReport", http.MethodGet, "/admin/talent/subjects/{user_id}/scores"),
		"listed operation outside /admin":  auditorOp("getTalentReport", http.MethodGet, "/talent/subjects/{user_id}/report"),
		"listed operation, other method":   auditorOp("getTalentReport", http.MethodPost, reportPath),
		"listed operation, other tag": func() huma.Operation {
			o := auditorOp("getTalentReport", http.MethodGet, reportPath)
			o.Tags = []string{"students"}
			return o
		}(),
		"listed operation, two tags": func() huma.Operation {
			o := auditorOp("getTalentReport", http.MethodGet, reportPath)
			o.Tags = []string{"talent", "students"}
			return o
		}(),
		"listed operation, other action": func() huma.Operation {
			o := auditorOp("getTalentReport", http.MethodGet, reportPath)
			o.Extensions["x-koro-action"] = "students.profile.read_admin"
			return o
		}(),
		"listed operation, no action": func() huma.Operation {
			o := auditorOp("getTalentReport", http.MethodGet, reportPath)
			delete(o.Extensions, "x-koro-action")
			return o
		}(),
		"listed operation, not the admin surface": func() huma.Operation {
			o := auditorOp("getTalentReport", http.MethodGet, reportPath)
			o.Extensions["x-koro-surface"] = "self"
			return o
		}(),
	} {
		if msg := register[guardEmpty, guardBand](guardApp(t), o); msg == "" || !strings.Contains(msg, "talent field") {
			t.Errorf("%s: registered (%q), want the talent field panic", name, msg)
		}
	}
	// The exemption is the operation's, not its schemas': a students
	// operation returning the report DTO is refused even after the auditor
	// operation registered the schema.
	api := guardApp(t)
	if msg := register[guardEmpty, guardReport](api, auditorOp("getTalentReport", http.MethodGet, reportPath)); msg != "" {
		t.Fatalf("talent_auditor operation rejected: %s", msg)
	}
	if msg := register[guardEmpty, guardReport](api, guardOp("leak", "students")); msg == "" ||
		!strings.Contains(msg, `TalentReportDTO member "band"`) {
		t.Fatalf("a students operation reusing the talent DTO registered (%q)", msg)
	}
}
