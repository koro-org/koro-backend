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

// SuggestionDTO mirrors payments' match suggestion (talentFieldExempt).
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

// TestTalentFieldGuard_AllowsTalentOperationsAndExemptions: the talent
// operations (BE-052, tag "talent") carry the band; size_band, bandwidth,
// activity_ranking_choice and the reasoned exemption pass.
func TestTalentFieldGuard_AllowsTalentOperationsAndExemptions(t *testing.T) {
	t.Parallel()
	api := guardApp(t)
	if msg := register[guardEmpty, guardBand](api, guardOp("report", "talent")); msg != "" {
		t.Fatalf("talent operation rejected: %s", msg)
	}
	if msg := register[guardEmpty, guardAllowed](api, guardOp("allowed", "orgs")); msg != "" {
		t.Fatalf("allowed members rejected: %s", msg)
	}
	if msg := register[guardEmpty, guardSuggestion](api, guardOp("suggest", "payments")); msg != "" {
		t.Fatalf("exempt SuggestionDTO.score rejected: %s", msg)
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
