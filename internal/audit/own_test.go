package audit_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/httpx"
)

// BE-078 (T-316): every refused sign-in to an account — a wrong password
// (invalid_credentials), an attempt on a locked account (locked), a wrong
// second factor (invalid_mfa_code) — is among the owner's security
// events, whoever made it, with its reason code but without the network
// facts of a request the owner did not make; nobody else's attempts are.
// Consecutive attempts with the same reason, each at most 15 minutes
// before the next, are one item counting them, so a burst cannot bury the
// owner's other events; one request reads at most 1000 rows and pages
// never show a row twice or skip one. Before BE-078 the anonymous
// attempts were missing from the list and no item carried a reason.

// The bodies run on the in-memory store here and on PostgreSQL in
// own_integration_test.go.

func TestListMySecurityEvents_SignInAttemptsWithoutNetworkFacts(t *testing.T) {
	t.Parallel()
	runSignInAttemptsVisible(t, newEnv(t, nil))
}

func TestListMySecurityEvents_AttemptBurstsSummarisedAcrossPages(t *testing.T) {
	t.Parallel()
	runAttemptBursts(t, newEnv(t, nil))
}

// signInAttempt is the record identity writes for a refused sign-in to
// account (createSession's password step, completeMfaLogin's code).
func signInAttempt(account uuid.UUID, reason string, by authz.Actor) audit.Event {
	return audit.Event{Action: string(authz.IdentitySessionCreate), Outcome: audit.Denied, ResourceType: authz.TypeUser,
		ResourceID: ptr(account), Reason: reason, Metadata: map[string]any{"failed_attempts": 1}}.By(by)
}

// ownEvents lists every security event of a (pages of 100).
func ownEvents(t *testing.T, e *env, a authz.Actor) []audit.SecurityEvent {
	t.Helper()
	var out []audit.SecurityEvent
	var after *audit.After
	for range 100 {
		p, err := e.svc.ListOwn(t.Context(), a, audit.OwnQuery{PageQuery: audit.PageQuery{Limit: 100, After: after}})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Items...)
		if p.Next == nil {
			return out
		}
		after = p.Next
	}
	t.Fatal("listMySecurityEvents never ended")
	return nil
}

func runSignInAttemptsVisible(t *testing.T, e *env) {
	t.Helper()
	me, other, support := persona(t, "student"), persona(t, "student_b"), persona(t, "support")
	mine := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), "198.51.100.77"), "Mozilla/5.0 (Macintosh) Version/19 Safari/605.1.15")
	theirs := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), "203.0.113.9"), "curl/8.9.1")
	for _, x := range []struct {
		ctx context.Context
		ev  audit.Event
	}{
		{mine, audit.Event{Action: string(authz.IdentitySessionCreate), Outcome: audit.Success, ResourceType: "session",
			ResourceID: ptr(uuid.NewV7())}.By(me)},
		{theirs, signInAttempt(me.UserID, "invalid_credentials", authz.Anonymous())},
		{theirs, signInAttempt(me.UserID, "locked", authz.Anonymous())},
		// From a session of another account: that account's own request.
		{theirs, signInAttempt(me.UserID, "invalid_mfa_code", other)},
		{theirs, signInAttempt(other.UserID, "invalid_credentials", authz.Anonymous())}, // another account
		// Not a sign-in: an anonymous refusal naming the account stays out.
		{theirs, audit.Event{Action: string(authz.IdentityEmailVerify), Outcome: audit.Denied, ResourceType: authz.TypeUser,
			ResourceID: ptr(me.UserID), Reason: "expired"}},
		{mine, audit.Event{Action: string(authz.IdentityPasswordChange), Outcome: audit.Denied, ResourceType: authz.TypeUser,
			ResourceID: ptr(me.UserID), Reason: audit.ReasonReauthFailed}.By(me)},
		// A justification typed by a person is free text, never shown.
		{mine, audit.Event{Action: string(authz.IdentityUserSuspend), Outcome: audit.Success, ResourceType: authz.TypeUser,
			ResourceID: ptr(other.UserID), Reason: "Kullanım koşulları ihlali"}.By(me)},
		{theirs, audit.Event{Action: "students.profile.read_admin", Outcome: audit.Success, Reason: "ticket SUP-1 investigation",
			Metadata: map[string]any{audit.MetaBreakGlass: true, audit.MetaSubjectUserID: me.UserID.String()}}.By(support)},
	} {
		if err := e.svc.Log(x.ctx, x.ev); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Hour) // no two attempts are one item
	}

	got := ownEvents(t, e, me)
	type view struct {
		action, outcome, reason, ip, ua string
		attempts                        int
	}
	var views []view
	for _, ev := range got {
		views = append(views, view{ev.Action, string(ev.Outcome), ev.Reason, ev.IPPrefix, ev.UserAgentFamily, ev.Attempts})
		if ev.Attempts > 0 && !ev.FirstOccurredAt.Equal(ev.OccurredAt) {
			t.Errorf("a single attempt %+v spans a period", ev)
		}
	}
	want := []view{ // newest first
		{"students.profile.read_admin", "success", "", "", "", 0}, // staff: no network, no justification
		{"identity.user.suspend", "success", "", "198.51.100.0/24", "Safari", 0},
		{"identity.password.change", "denied", "reauth_failed", "198.51.100.0/24", "Safari", 0},
		{"identity.session.create", "denied", "invalid_mfa_code", "", "", 1},
		{"identity.session.create", "denied", "locked", "", "", 1},
		{"identity.session.create", "denied", "invalid_credentials", "", "", 1},
		{"identity.session.create", "success", "", "198.51.100.0/24", "Safari", 0},
	}
	if fmt.Sprint(views) != fmt.Sprint(want) {
		t.Fatalf("owner's security events\n got %v\nwant %v", views, want)
	}

	// The other account sees the attempt on it and, as its own request with
	// its own network, the attempt it made on mine.
	var attempts, own int
	for _, ev := range ownEvents(t, e, other) {
		switch {
		case ev.Action != string(authz.IdentitySessionCreate):
		case ev.Attempts == 1 && ev.Reason == "invalid_credentials" && ev.IPPrefix == "":
			attempts++
		case ev.Attempts == 0 && ev.Reason == "invalid_mfa_code" && ev.IPPrefix == "203.0.113.0/24":
			own++
		default:
			t.Errorf("other account: unexpected sign-in item %+v", ev)
		}
	}
	if attempts != 1 || own != 1 {
		t.Fatalf("other account: %d attempts on it, %d own attempts; want 1 and 1", attempts, own)
	}
}

// burst writes n anonymous attempts with reason on account, step apart.
func (e *env) burst(t *testing.T, account uuid.UUID, reason string, n int, step time.Duration) {
	t.Helper()
	for range n {
		e.clk.Advance(step)
		if err := e.svc.Log(t.Context(), signInAttempt(account, reason, authz.Anonymous())); err != nil {
			t.Fatal(err)
		}
	}
}

func runAttemptBursts(t *testing.T, e *env) {
	t.Helper()
	me := persona(t, "student")
	own := func(act authz.Action) {
		t.Helper()
		e.clk.Advance(time.Minute)
		if err := e.svc.Log(t.Context(), audit.Event{Action: string(act), Outcome: audit.Success, ResourceType: authz.TypeUser,
			ResourceID: ptr(me.UserID)}.By(me)); err != nil {
			t.Fatal(err)
		}
	}
	own(authz.IdentitySessionCreate)                             // A
	e.burst(t, me.UserID, "invalid_credentials", 1, time.Minute) // B
	e.clk.Advance(18 * time.Minute)                              // 19 minutes after B: a new item
	e.burst(t, me.UserID, "invalid_credentials", 5, 10*time.Second)
	e.burst(t, me.UserID, "locked", 1500, 2*time.Second) // a burst longer than one request reads
	own(authz.IdentityPasswordChange)                    // F: must not be buried

	type item struct {
		action, reason string
		attempts       int
	}
	canonical := []item{
		{"identity.password.change", "", 0}, {"identity.session.create", "locked", 1500},
		{"identity.session.create", "invalid_credentials", 5}, {"identity.session.create", "invalid_credentials", 1},
		{"identity.session.create", "", 0},
	}

	// limit 10: the burst fills the read budget of the first request.
	q := audit.OwnQuery{PageQuery: audit.PageQuery{Limit: 10}}
	p1, err := e.svc.ListOwn(t.Context(), me, q)
	if err != nil {
		t.Fatal(err)
	}
	q.After = p1.Next
	p2, err := e.svc.ListOwn(t.Context(), me, q)
	if err != nil {
		t.Fatal(err)
	}
	summary := func(p audit.Page[audit.SecurityEvent]) string {
		var parts []string
		for _, ev := range p.Items {
			parts = append(parts, fmt.Sprintf("%s/%s×%d", ev.Action, ev.Reason, ev.Attempts))
		}
		return fmt.Sprintf("%s next=%v", strings.Join(parts, " "), p.Next != nil)
	}
	if got, want := summary(p1), "identity.password.change/×0 identity.session.create/locked×999 next=true"; got != want {
		t.Fatalf("first page %s\nwant %s", got, want)
	}
	if got, want := summary(p2), "identity.session.create/locked×501 identity.session.create/invalid_credentials×5 "+
		"identity.session.create/invalid_credentials×1 identity.session.create/×0 next=false"; got != want {
		t.Fatalf("second page %s\nwant %s", got, want)
	}
	if b := p1.Items[1]; !b.OccurredAt.After(b.FirstOccurredAt) || b.OccurredAt.Sub(b.FirstOccurredAt) != 998*2*time.Second {
		t.Fatalf("burst item %v … %v, want 999 attempts 2 s apart", b.FirstOccurredAt, b.OccurredAt)
	}

	// Any page size: no empty page, at most limit items, and the pages put
	// together (an item split by the read budget joined again) are the
	// canonical list — no row twice, none skipped.
	for _, limit := range []int{1, 2, 3, 7, 100} {
		var all []audit.SecurityEvent
		q := audit.OwnQuery{PageQuery: audit.PageQuery{Limit: limit}}
		for pages := 0; ; pages++ {
			if pages > 50 {
				t.Fatalf("limit %d: no end", limit)
			}
			p, err := e.svc.ListOwn(t.Context(), me, q)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Items) == 0 || len(p.Items) > limit {
				t.Fatalf("limit %d: page %d has %d items", limit, pages+1, len(p.Items))
			}
			for _, ev := range p.Items {
				n := len(all)
				if n > 0 && all[n-1].Attempts > 0 && ev.Attempts > 0 && all[n-1].Reason == ev.Reason &&
					all[n-1].FirstOccurredAt.Sub(ev.OccurredAt) <= 15*time.Minute {
					if !ev.OccurredAt.Before(all[n-1].FirstOccurredAt) {
						t.Fatalf("limit %d: overlapping attempt items %+v, %+v", limit, all[n-1], ev)
					}
					all[n-1].Attempts += ev.Attempts
					all[n-1].FirstOccurredAt = ev.FirstOccurredAt
					continue
				}
				all = append(all, ev)
			}
			if p.Next == nil {
				break
			}
			q.After = p.Next
		}
		var got []item
		for _, ev := range all {
			got = append(got, item{ev.Action, ev.Reason, ev.Attempts})
		}
		if fmt.Sprint(got) != fmt.Sprint(canonical) {
			t.Fatalf("limit %d: pages put together\n got %v\nwant %v", limit, got, canonical)
		}
	}
}
