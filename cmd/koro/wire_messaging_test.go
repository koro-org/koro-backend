package main

import (
	"fmt"
	"slices"
	"testing"

	"koro/internal/messaging"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/privacy"
)

// useMemStore installs the in-memory messaging stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-056.
// Only tickets of the messaging module edit this file (BE-001 R4).
// created_at and updated_at use the real clock (matrix fixtures put their
// own rows).
func (w *messagingWiring) useMemStore() {
	w.store = messaging.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_MessagingProvidesServiceTasksAndPrivacy (BE-056): messaging
// builds over discovery's pseudonyms and hook, students, entitlements,
// orgs, feed and audit; registers its KVKK provider and the
// contact_request_closed sweeper, the expiry job of 01 §7 and the two
// notice subscriptions of 01 §6.2, and its 10 operations with their
// surfaces, rate class and idempotency.
func TestWire_MessagingProvidesServiceTasksAndPrivacy(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.messaging.svc == nil {
		t.Fatal("messaging did not set its service")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "messaging" }) {
		t.Fatal("messaging privacy provider not registered")
	}
	claimed := false
	for _, s := range c.sweepers {
		claimed = claimed || slices.Contains(s.PolicyCodes(), messaging.PolicyContactRequestClosed)
	}
	if !claimed {
		t.Fatal("contact_request_closed has no sweeper")
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if j.Name == messaging.ExpireJobName {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"messaging.expire_requests 40 * * * * default"}; !slices.Equal(jobs, want) {
		t.Fatalf("messaging jobs %q, want %q (01 §7)", jobs, want)
	}
	subs := map[string]string{}
	for _, s := range c.infra.subs.All() {
		if s.Name == messaging.NotifyReceivedTaskName || s.Name == messaging.NotifyClosedTaskName {
			subs[s.Name] = s.Topic
		}
	}
	if subs[messaging.NotifyReceivedTaskName] != domainevent.TopicContactRequestCreated ||
		subs[messaging.NotifyClosedTaskName] != domainevent.TopicContactRequestClosed {
		t.Fatalf("messaging subscriptions %v", subs)
	}

	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: c.actors})
	if err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	paths := app.API.OpenAPI().Paths
	for _, tc := range []struct{ method, path, id, surface string }{
		{"POST", "/api/v1/orgs/{org_id}/contact-requests", "createContactRequest", "org"},
		{"GET", "/api/v1/orgs/{org_id}/contact-requests", "listSentContactRequests", "org"},
		{"GET", "/api/v1/orgs/{org_id}/contact-requests/{request_id}", "getSentContactRequest", "org"},
		{"POST", "/api/v1/orgs/{org_id}/contact-requests/{request_id}/withdraw", "withdrawContactRequest", "org"},
		{"GET", "/api/v1/me/contact-requests", "listMyContactRequests", "self"},
		{"GET", "/api/v1/me/contact-requests/{request_id}", "getMyContactRequest", "self"},
		{"POST", "/api/v1/me/contact-requests/{request_id}/respond", "respondMyContactRequest", "self"},
		{"PUT", "/api/v1/me/company-blocks/{org_id}", "blockCompany", "self"},
		{"DELETE", "/api/v1/me/company-blocks/{org_id}", "unblockCompany", "self"},
		{"GET", "/api/v1/me/company-blocks", "listMyCompanyBlocks", "self"},
	} {
		p := paths[tc.path]
		if p == nil {
			t.Fatalf("%s: path not registered", tc.path)
		}
		op := p.Get
		switch tc.method {
		case "POST":
			op = p.Post
		case "PUT":
			op = p.Put
		case "DELETE":
			op = p.Delete
		}
		if op == nil || op.OperationID != tc.id {
			t.Fatalf("%s %s: operation %s not registered", tc.method, tc.path, tc.id)
		}
		if got := fmt.Sprint(op.Extensions["x-koro-surface"]); got != tc.surface {
			t.Fatalf("%s: surface %s, want %s", tc.id, got, tc.surface)
		}
	}
	create := paths["/api/v1/orgs/{org_id}/contact-requests"].Post
	if rl := fmt.Sprint(create.Extensions["x-koro-rate-limit"]); rl != "contact_request" {
		t.Fatalf("createContactRequest rate class = %s, want contact_request (04 §1.9)", rl)
	}
	if idem := fmt.Sprint(create.Extensions["x-koro-idempotency"]); idem != "required" {
		t.Fatalf("createContactRequest idempotency = %s, want required (04 §1.8)", idem)
	}
}
