package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/discovery"
	"koro/internal/students"
)

// matrixPseudonyms are the pseudonyms the discovery matrix addresses as
// path literals (testdata/authz/discovery.yaml): "<company>/<student>" →
// the anon_id the shared test deriver (key test1) derives.
// TestMatrixDiscovery_PseudonymLiterals keeps them in step with the code.
var matrixPseudonyms = map[string]string{
	"company_a/student_granted": "l7fbt3m4pxse7dc2",
	"company_b/student_granted": "ael3ea4fuqkpycbd",
	"company_a/student":         "mjh6rq23ztj3hxgo",
}

// matrixFixtures seeds fixtures/discovery.yaml (BE-051): a "discoverable"
// fixture turns level 2 on for a helper student the way the student would
// (updateMyPrivacy with the published discovery_profile text of students'
// fixture notice_discovery), so the read-time checks (verification, age,
// account, consent) pass for real.
func (w *discoveryWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	for _, f := range fx.Of("discovery") {
		switch f.Kind {
		case "discoverable":
			name, _ := f.Fields["student"].(string)
			if err := w.matrixLevelTwo(ctx, name, true); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}

// matrixHook runs the discovery scenario hooks: "hide" turns level 2 off
// for args.student (the student leaves discovery).
func (w *discoveryWiring) matrixHook(_ *matrixFixtures, name string, args map[string]any) error {
	switch name {
	case "hide":
		student, _ := args["student"].(string)
		return w.matrixLevelTwo(context.Background(), student, false)
	}
	return fmt.Errorf("discovery: unknown hook %q", name)
}

// matrixLevelTwo turns level 2 on (with the consent) or off for a helper
// student, creating the profile when the student has none.
func (w *discoveryWiring) matrixLevelTwo(ctx context.Context, name string, on bool) error {
	p, ok := authztest.Get(name)
	if !ok || p.Actor.UniversityID == nil || p.Actor.IsMinor {
		return fmt.Errorf("student %q must be a verified adult student", name)
	}
	c := w.late
	if c == nil || c.students.svc == nil || c.students.store == nil {
		return errors.New("students is not built")
	}
	if _, _, err := c.students.store.InsertProfile(ctx, students.Profile{UserID: p.User(), UniversityID: *p.Actor.UniversityID,
		DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}); err != nil {
		return err
	}
	patch := students.PrivacyPatch{Discoverable: &on, Channel: consent.ChannelWeb}
	if on {
		patch.DiscoveryConsent = &students.ConsentProof{NoticeVersionID: authztest.ID("notice_discovery"),
			TextSHA256: consent.BodyDigest(matrixDiscoveryText)}
	}
	_, err := c.students.svc.UpdateMyPrivacy(ctx, p.Actor, patch)
	return err
}

// TestMatrixDiscovery_PseudonymLiterals: every pseudonym literal of the
// discovery matrix is the one discovery derives with the test deriver, and
// every anon_id literal of discovery.yaml is one of them (or a deliberately
// malformed one).
func TestMatrixDiscovery_PseudonymLiterals(t *testing.T) {
	t.Parallel()
	ps, err := discovery.NewPseudonyms(testDeriver(), []string{"test1"})
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for pair, want := range matrixPseudonyms {
		org, student, _ := strings.Cut(pair, "/")
		got, err := ps.AnonID(authztest.ID(org), authztest.ID(student))
		if err != nil || got != want {
			t.Errorf("%s: pseudonym %q, %v; matrixPseudonyms says %q", pair, got, err, want)
		}
		known[want] = true
	}
	raw, err := fs.ReadFile(os.DirFS("testdata/authz"), "discovery.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Operations map[string]struct {
			PathParams map[string]string `yaml:"path_params"`
			Variants   []struct {
				PathParams map[string]string `yaml:"path_params"`
			} `yaml:"variants"`
		} `yaml:"operations"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	check := func(params map[string]string) {
		v, ok := params["anon_id"]
		if !ok {
			return
		}
		lit, _ := strings.CutPrefix(v, literalPrefix)
		if !known[lit] && discovery.ValidAnonID(lit) {
			t.Errorf("anon_id %q is a well-formed pseudonym that is not in matrixPseudonyms", lit)
		}
	}
	for _, op := range doc.Operations {
		check(op.PathParams)
		for _, v := range op.Variants {
			check(v.PathParams)
		}
	}
}

// TestMatrixDiscovery_RealModulesAnswer (BE-051): on the matrix
// application (real students, identity, consent, entitlements, orgs,
// talent and audit over the in-memory stores) a search of company_a finds
// one discoverable student — below 10, an empty list — and its pseudonym
// reads the level 2 profile without the user ID; hooks.anonResolver maps
// the pseudonym back only for its own company.
func TestMatrixDiscovery_RealModulesAnswer(t *testing.T) {
	t.Parallel()
	env := &matrixEnv{opts: realMatrixOptions()}
	m, probs := loadMatrix(os.DirFS("testdata/authz"), env.opts.modules)
	if len(probs) > 0 {
		t.Fatal(strings.Join(probs, "\n"))
	}
	env.m = m
	a := env.newMatrixApp(t, m.config)
	student := authztest.ID("student_granted")
	call := func(path string, params map[string]string) (int, string) {
		return a.do(t, m, matrixCase{name: "discovery " + path, method: "GET", path: path,
			req: matrixRequest{PathParams: params}, persona: "company_member"})
	}
	code, body := call("/api/v1/orgs/{org_id}/discovery/profiles", map[string]string{"org_id": "company_a"})
	if code != 200 || !strings.Contains(body, `"items":[]`) || !strings.Contains(body, `"below_threshold":true`) {
		t.Fatalf("search = %d %s", code, body)
	}
	anon := matrixPseudonyms["company_a/student_granted"]
	code, body = call("/api/v1/orgs/{org_id}/discovery/profiles/{anon_id}",
		map[string]string{"org_id": "company_a", "anon_id": literalPrefix + anon})
	if code != 200 || !strings.Contains(body, `"anon_id":"`+anon+`"`) || strings.Contains(body, student.String()) ||
		strings.Contains(body, "user_id") {
		t.Fatalf("read = %d %s", code, body)
	}
	if id, found, err := a.c.hooks.anonResolver(t.Context(), authztest.ID("company_a"), anon); err != nil || !found || id != student {
		t.Fatalf("anonResolver = %v, %v, %v", id, found, err)
	}
	if _, found, err := a.c.hooks.anonResolver(t.Context(), authztest.ID("company_b"), anon); err != nil || found {
		t.Fatalf("company_a's pseudonym resolved for company_b: %v, %v", found, err)
	}
}
