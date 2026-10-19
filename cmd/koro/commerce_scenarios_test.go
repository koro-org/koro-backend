package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestAuthzScenarios_AllFourEyesCovered is a runnable inventory of 03 §6.1
// and H-17/H-47. Removing or renaming one mandatory scenario fails before
// any scenario runs; new four-eye scenarios are selected automatically.
// Each selected YAML executes its real positive and negative HTTP steps on
// the existing isolated matrix application (not merely a text/name check).
func TestAuthzScenarios_AllFourEyesCovered(t *testing.T) {
	t.Parallel()
	env := &matrixEnv{opts: realMatrixOptions()}
	m, problems := loadMatrix(os.DirFS("testdata/authz"), env.opts.modules)
	if len(problems) != 0 {
		t.Fatalf("matrix load: %v", problems)
	}
	if !m.strict {
		t.Fatal("BE-045 / H-29 requires strict_action_coverage")
	}
	env.m = m
	required := []string{
		"offering_four_eyes", "invoice_four_eyes", "payout_four_eyes",
		"receivable_collection_four_eyes", "refund_closure_four_eyes", "refund_failed_four_eyes",
		"collection_account_four_eyes", "transfer_return_four_eyes", "refund_four_eyes", "refund_four_eyes_platform_admin",
		"document_four_eyes", "iban_four_eyes_org_submission", "iban_four_eyes_admin_submission", "dsr_profiling_four_eyes",
		"four_eyes_grant", "four_eyes_revoke", "four_eyes_suspend_privileged", "four_eyes_revoke_sessions_privileged", "four_eyes_reset_mfa_privileged",
	}
	var selected []*scenario
	for _, s := range m.scenarios {
		fourEyes := strings.Contains(s.name, "four_eyes") || slices.ContainsFunc(s.Steps, func(st *scenarioStep) bool { return st.Code == "authz.four_eyes_violation" })
		if fourEyes {
			selected = append(selected, s)
		}
	}
	var missing []string
	for _, name := range required {
		if !slices.ContainsFunc(selected, func(s *scenario) bool { return s.name == name }) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("missing four-eye scenarios: %s", strings.Join(missing, ", "))
	}
	spec, routes := env.specAndRoutes(t)
	if problems, _ := checkMatrix(env.checkInput(spec, routes)); len(problems) != 0 {
		t.Fatalf("matrix contracts: %v", problems)
	}
	for _, s := range selected {
		positive, negative := false, false
		for _, st := range s.Steps {
			positive = positive || (st.Expect >= 200 && st.Expect < 300)
			negative = negative || (st.Expect == 403)
		}
		if !positive || !negative {
			t.Fatalf("%s needs both a permitted step and a forbidden step", s.name)
		}
		t.Run(s.name, func(t *testing.T) { t.Parallel(); env.runScenario(t, s) })
	}
}
