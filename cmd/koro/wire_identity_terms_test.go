package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// refuseTerms is a terms checker that refuses everybody and counts.
type refuseTerms struct{ calls atomic.Int64 }

func (r *refuseTerms) CheckTerms(context.Context, uuid.UUID) error {
	r.calls.Add(1)
	return consent.ErrTermsAcceptanceRequired
}

// termsAlwaysOpen are the actions of the KVKK and account security
// operations (BE-124): a self-surface write operation of one of them (the
// user acting on their own account and data) must be on the terms gate's
// exemption table, so a new operation of the family (another MFA method, a
// new DSR entry point, a new consent page) cannot end up behind the gate
// by default. Actions shared with gated operations
// (identity.account.update_own: acceptTerms and updateMe;
// notify.push_token.manage: registering and deleting a device) are pinned
// by operation in the table instead. An org's own act under such an action
// (revokeCompanyAccessGrant: the company gives up a grant) is not the
// data subject's right and stays gated.
var termsAlwaysOpen = []string{
	"identity.session.delete_current", "identity.session.revoke_own",
	"identity.mfa.enroll", "identity.mfa.confirm", "identity.mfa.disable", "identity.mfa.verify", "identity.mfa_recovery.regenerate",
	"identity.password.change", "identity.email_change.request",
	"kvkk.dsr.create", "kvkk.dsr.cancel_own",
	"consent.record.create", "students.privacy.update_own", "students.access_grant.revoke",
}

var pathParam = regexp.MustCompile(`\{[a-z_]+\}`)

// TestTermsGate_EveryProtectedWriteIsGatedOrExempt (BE-124) runs the real
// application (every module's routes, the operation middlewares of `koro
// api`) with a checker that refuses everybody. Every protected write
// operation is marked x-koro-terms-gate and answers as marked: enforced →
// 403 consent.terms_acceptance_required before its handler; exempt → its
// handler runs (whatever it answers to an empty request, never the gate's
// code, and the checker is not asked). Reads, public operations and
// webhooks carry no mark and are never asked about. Every exemption row
// names a registered operation, and the KVKK and account security actions
// are all exempt. A new write operation is therefore gated unless it is
// added to the table with its reason.
func TestTermsGate_EveryProtectedWriteIsGatedOrExempt(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	res := authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)
	c.actors = res
	terms := &refuseTerms{}
	c.terms = terms
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.terms != terms {
		t.Fatal("identity replaced a preset terms checker")
	}
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Version: apiVersion, Actors: c.actors, RequestTimeout: matrixRequestTimeout})
	if err != nil {
		t.Fatal(err)
	}
	c.apiMiddleware(app.API, false)
	c.registerRoutes(app.V1)
	student, _ := authztest.Get("student")

	if missing := httpx.TermsExemptionsUnregistered(app.API.OpenAPI()); len(missing) > 0 {
		t.Fatalf("exemption rows without their operation: %v", missing)
	}
	exempt := httpx.TermsExemptOperations()
	var enforced, exempted int
	for path, item := range app.API.OpenAPI().Paths {
		for _, o := range []*huma.Operation{item.Get, item.Head, item.Post, item.Put, item.Patch, item.Delete} {
			if o == nil {
				continue
			}
			mark, _ := o.Extensions[httpx.ExtTermsGate].(string)
			write := len(o.Security) > 0 && o.Method != http.MethodGet && o.Method != http.MethodHead
			action, _ := o.Extensions["x-koro-action"].(string)
			switch {
			case !write:
				if mark != "" {
					t.Errorf("%s (%s %s): marked %q but not a protected write", o.OperationID, o.Method, path, mark)
				}
				continue
			case exempt[o.OperationID] == o.Method+" "+path:
				if mark != httpx.TermsGateExempt {
					t.Errorf("%s: listed exempt, marked %q", o.OperationID, mark)
				}
				exempted++
			default:
				if mark != httpx.TermsGateEnforced {
					t.Errorf("%s: gated, marked %q", o.OperationID, mark)
				}
				if slices.Contains(termsAlwaysOpen, action) && o.Extensions["x-koro-surface"] == "self" {
					t.Errorf("%s (%s): a KVKK or account security operation behind the terms gate: add it to "+
						"httpx termsExemptOperations with its reason", o.OperationID, action)
				}
				enforced++
			}

			before := terms.calls.Load()
			url := pathParam.ReplaceAllStringFunc(path, func(string) string { return uuid.NewV7().String() })
			req := httptest.NewRequestWithContext(t.Context(), o.Method, url, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+student.Token)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			app.Handler().ServeHTTP(rec, req)
			var body struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			asked := terms.calls.Load() - before
			if mark == httpx.TermsGateEnforced {
				if rec.Code != http.StatusForbidden || body.Code != "consent.terms_acceptance_required" || asked != 1 {
					t.Errorf("%s %s (%s) = %d %s (checker asked %d), want 403 consent.terms_acceptance_required",
						o.Method, path, o.OperationID, rec.Code, body.Code, asked)
				}
			} else if body.Code == "consent.terms_acceptance_required" || asked != 0 {
				t.Errorf("%s %s (%s, exempt) = %d %s (checker asked %d)", o.Method, path, o.OperationID, rec.Code, body.Code, asked)
			}
		}
	}
	if exempted != len(exempt) || enforced == 0 {
		t.Fatalf("%d exempt operations of %d rows, %d enforced", exempted, len(exempt), enforced)
	}
	t.Logf("terms gate: %d protected write operations enforced, %d exempt", enforced, exempted)
}

// TestWire_TermsGateOverHTTP (BE-124, Q-17): end to end over the wired
// modules. An account older than the terms is refused every write
// (403 consent.terms_acceptance_required) but reads its terms state and
// accepts them; then it writes. A new version published by the DPO through
// consent is enforced at once by this process (identity's watcher drops
// the cached version) and announced in the worker: the account gets the
// in-app notification (link /me/terms) and the e-mail; after accepting it
// writes again.
func TestWire_TermsGateOverHTTP(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	ctx := t.Context()
	if e.c.terms == nil {
		t.Fatal("identity did not bind the terms gate")
	}
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: e.c.actors})
	if err != nil {
		t.Fatal(err)
	}
	e.c.apiMiddleware(app.API, false)
	e.c.registerRoutes(app.V1)
	u := e.user(t, nil)
	tok := e.session(t, u, identity.ClientMobile)
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Koro-Client", "mobile")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	rename := func(want int) {
		t.Helper()
		code, body := do(http.MethodPatch, "/api/v1/me", `{"first_name":"Bea"}`)
		if code != want || want == http.StatusForbidden && body["code"] != "consent.terms_acceptance_required" {
			t.Fatalf("updateMe = %d %v, want %d", code, body, want)
		}
	}
	accept := func(id uuid.UUID) {
		t.Helper()
		if code, body := do(http.MethodPost, "/api/v1/me/terms-acceptances", `{"notice_version_id":"`+id.String()+`"}`); code != http.StatusOK {
			t.Fatalf("acceptTerms = %d %v", code, body)
		}
	}
	dpo, _ := authztest.Get("dpo")
	publish := func(version string) consent.Notice {
		t.Helper()
		n, err := e.c.consent.svc.CreateNotice(ctx, dpo.Actor, consent.NoticeDraft{Kind: consent.KindTermsOfUse,
			Audience: consent.AudienceAll, Locale: consent.LocaleTR, Version: version, Title: "Kullanım koşulları",
			BodyMD: "Kullanım koşulları " + version + "\n"}, consent.ReauthProof{Password: matrixReauthPassword})
		if err != nil {
			t.Fatal(err)
		}
		if n, err = e.c.consent.svc.PublishNotice(ctx, dpo.Actor, n.ID, consent.ReauthProof{Password: matrixReauthPassword}); err != nil {
			t.Fatal(err)
		}
		return n
	}

	rename(http.StatusOK) // no terms published: nothing to accept
	v1 := publish("2026.10.01")
	rename(http.StatusForbidden) // the account predates the terms
	if code, body := do(http.MethodGet, "/api/v1/me", ""); code != http.StatusOK ||
		body["terms"].(map[string]any)["acceptance_required"] != true {
		t.Fatalf("getMe = %d %v", code, body["terms"])
	}
	accept(v1.ID)
	rename(http.StatusOK)

	// A new version: refused at once, announced by the worker.
	q := e.c.infra.queue.(*queue.Mem)
	q.Reset()
	v2 := publish("2026.11.01")
	rename(http.StatusForbidden)
	runTask := func(typ string) int {
		t.Helper()
		n := 0
		for _, task := range e.c.infra.jobs.Tasks() {
			if task.Name != typ {
				continue
			}
			for _, x := range q.Tasks() {
				if x.Type == typ {
					if err := task.Handle(workerContext(t), x.Payload); err != nil {
						t.Fatalf("%s: %v", typ, err)
					}
					n++
				}
			}
		}
		return n
	}
	if runTask(identity.NotifyTermsPublishedTaskName) != 1 || runTask(identity.SendTermsNoticeTaskName) != 1 {
		t.Fatalf("worker tasks after the publication: %+v", q.Tasks())
	}
	code, body := do(http.MethodGet, "/api/v1/me/notifications", "")
	items, _ := body["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["link_path"] != "/me/terms" {
		t.Fatalf("listMyNotifications = %d %v", code, body)
	}
	sent := e.c.notify.transport.(*notify.MemTransport).Sent()
	if len(sent) != 1 || sent[0].To != u.Email || !strings.Contains(sent[0].Text, "2026.11.01") ||
		!strings.Contains(sent[0].Text, "https://app.koro.test/me/terms") {
		t.Fatalf("e-mails = %+v", sent)
	}
	accept(v2.ID)
	rename(http.StatusOK)
}
