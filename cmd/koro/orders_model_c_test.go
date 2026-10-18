package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The matrix checks all 18 personas' status codes; this also inspects the
// sensitive projection, so a 200 staff/member response cannot leak IBAN.
func TestOrders_ModelCIBANMatrix(t *testing.T) {
	t.Parallel()
	env := &matrixEnv{opts: realMatrixOptions()}
	m, problems := loadMatrix(os.DirFS("testdata/authz"), env.opts.modules)
	if len(problems) > 0 {
		t.Fatalf("matrix: %v", problems)
	}
	env.m = m
	count := 0
	for _, c := range env.cases() {
		if !strings.Contains(c.name, "direct_payee") {
			continue
		}
		count++
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			status, body := env.runCase(t, c)
			if status != c.want {
				t.Fatalf("status %d want %d: %s", status, c.want, body)
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(body), &obj); err != nil {
				t.Fatal(err)
			}
			direct, exists := obj["direct_payments"]
			if c.persona == "company_admin" {
				if !exists || len(direct.([]any)) != 1 {
					t.Fatalf("buyer admin instructions: %s", body)
				}
				p := direct.([]any)[0].(map[string]any)
				if p["iban"] != "TR330006100519786457841326" || p["direct_to_payee_amount"] != float64(1_200_000) {
					t.Fatalf("payee: %v", p)
				}
			} else if exists {
				t.Fatalf("payee IBAN exposed to %s: %s", c.persona, body)
			}
		})
	}
	if count != 36 {
		t.Fatalf("direct payee cases=%d, want 36", count)
	}
}
