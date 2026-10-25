package official_test

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/iys"
	"koro/internal/platform/iys/iyscontract"
	"koro/internal/platform/iys/iystest"
	"koro/internal/platform/iys/official"
)

// newClient builds the adapter over the double with the suite's directory.
func newClient(t *testing.T, srv *iystest.Server, clk clock.Clock, dir official.Recipients, mut ...func(*official.Config)) *official.Client {
	t.Helper()
	c := official.Config{BaseURL: srv.URL, Username: iystest.Username, Password: iystest.Password,
		IYSCode: iystest.IYSCode, BrandCode: iystest.BrandCode, Recipients: dir, Clock: clk}
	for _, m := range mut {
		m(&c)
	}
	client, err := official.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestOfficial_Contract: the official adapter over the İYS double passes
// the contract every adapter passes.
func TestOfficial_Contract(t *testing.T) {
	t.Parallel()
	iyscontract.Run(t, func(t *testing.T, env iyscontract.Env) iyscontract.Harness {
		srv := iystest.New(t, iystest.WithClock(env.Clock))
		return iyscontract.Harness{
			Adapter: newClient(t, srv, env.Clock, env.Recipients), Lag: time.Hour,
			Act: func(_ *testing.T, address string, typ iys.Type, st iys.Status) {
				srv.Act(iystest.Action{Recipient: address, Type: string(typ), Status: string(st)})
			},
			Held: func(_ *testing.T, address string, typ iys.Type) (iys.State, bool) {
				status, at, source, ok := srv.Held(address, string(typ))
				return iys.State{Status: iys.Status(status), At: at, Source: source}, ok
			},
			SetDown: srv.SetDown,
		}
	})
}

func approve(t *testing.T, c *official.Client, user uuid.UUID, at time.Time) {
	t.Helper()
	if _, err := c.Submit(t.Context(), iys.Consent{Ref: "sub", UserID: user, Type: iys.Email, Status: iys.Approved, Source: "HS_WEB",
		ConsentDate: at}); err != nil {
		t.Fatal(err)
	}
}

// TestOfficial_TokenCacheRefreshAndBudget: the access token is reused for
// two hours, renewed with the refresh token, then with the password once
// the refresh token expired; a 401 renews once; renewals beyond the hourly
// budget fail as throttled without reaching İYS.
func TestOfficial_TokenCacheRefreshAndBudget(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	srv := iystest.New(t, iystest.WithClock(clk))
	dir := iyscontract.NewDirectory()
	ada := dir.Add("ada.yilmaz@ogrenci.example.test")
	c := newClient(t, srv, clk, dir, func(c *official.Config) { c.SignInsPerHour = 3 })

	approve(t, c, ada, clk.Now())
	clk.Advance(time.Hour)
	approve(t, c, ada, clk.Now())
	if srv.SignIns() != 1 || srv.Refreshes() != 0 {
		t.Fatalf("sign-ins %d, refreshes %d; want the cached token", srv.SignIns(), srv.Refreshes())
	}
	clk.Advance(time.Hour + time.Minute) // access token expired, refresh token valid
	approve(t, c, ada, clk.Now())
	if srv.SignIns() != 1 || srv.Refreshes() != 1 {
		t.Fatalf("sign-ins %d, refreshes %d; want one refresh", srv.SignIns(), srv.Refreshes())
	}
	clk.Advance(5 * time.Hour) // both expired
	approve(t, c, ada, clk.Now())
	if srv.SignIns() != 2 {
		t.Fatalf("sign-ins %d, want a password sign-in after the refresh token expired", srv.SignIns())
	}

	// 401 → one renewal; then the budget of 3 per hour runs out.
	srv.ExpireTokens()
	approve(t, c, ada, clk.Now())
	srv.ExpireTokens()
	approve(t, c, ada, clk.Now())
	tokenCalls := srv.Calls("token")
	srv.ExpireTokens()
	_, err := c.Submit(t.Context(), iys.Consent{Ref: "sub", UserID: ada, Type: iys.Email, Status: iys.Approved, Source: "HS_WEB",
		ConsentDate: clk.Now()})
	if !errors.Is(err, iys.ErrThrottled) || srv.Calls("token") != tokenCalls {
		t.Fatalf("renewal beyond the budget = %v (token calls %d → %d), want throttled without a call", err, tokenCalls, srv.Calls("token"))
	}
	clk.Advance(time.Hour)
	approve(t, c, ada, clk.Now())
}

// TestOfficial_TokenLimitPausesSignIns: a 429 of the token endpoint (H084)
// stops sign-ins for an hour: a retry would restart İYS's count.
func TestOfficial_TokenLimitPausesSignIns(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	srv := iystest.New(t, iystest.WithClock(clk), iystest.WithTokenLimit(1))
	dir := iyscontract.NewDirectory()
	ada := dir.Add("ada.yilmaz@ogrenci.example.test")
	c := newClient(t, srv, clk, dir)
	approve(t, c, ada, clk.Now())
	srv.RevokeTokens()
	consent := iys.Consent{Ref: "sub", UserID: ada, Type: iys.Email, Status: iys.Approved, Source: "HS_WEB", ConsentDate: clk.Now()}
	if _, err := c.Submit(t.Context(), consent); !errors.Is(err, iys.ErrThrottled) || !strings.Contains(err.Error(), "H084") {
		t.Fatalf("token request over İYS's limit = %v, want throttled H084", err)
	}
	calls := srv.Calls("token")
	clk.Advance(30 * time.Minute)
	if _, err := c.Submit(t.Context(), consent); !errors.Is(err, iys.ErrThrottled) || srv.Calls("token") != calls {
		t.Fatalf("within the pause = %v (token calls %d → %d)", err, calls, srv.Calls("token"))
	}
	clk.Advance(31 * time.Minute)
	approve(t, c, ada, clk.Now())
}

// TestOfficial_StatusesBulkFirstThenSingleWithinBudget: the bulk query
// answers the approvals; only the other addresses cost a single query,
// within the adapter's hourly budget; İYS's H086 pauses single queries for
// an hour.
func TestOfficial_StatusesBulkFirstThenSingleWithinBudget(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	srv := iystest.New(t, iystest.WithClock(clk), iystest.WithStatusLimit(2))
	dir := iyscontract.NewDirectory()
	c := newClient(t, srv, clk, dir)
	var users []uuid.UUID
	for i, a := range []string{"a@ogrenci.example.test", "b@ogrenci.example.test", "c@ogrenci.example.test", "d@ogrenci.example.test"} {
		u := dir.Add(a)
		users = append(users, u)
		if i < 3 {
			approve(t, c, u, clk.Now())
		}
	}
	srv.Act(iystest.Action{Recipient: "c@ogrenci.example.test"}) // refused in İYS
	got, err := c.Statuses(t.Context(), iys.Email, users)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[users[0]].Status != iys.Approved || got[users[2]].Status != iys.Refused || srv.Calls("bulk") != 1 ||
		srv.Calls("status") != 2 {
		t.Fatalf("statuses %+v with %d bulk and %d single queries; want 1 and 2", got, srv.Calls("bulk"), srv.Calls("status"))
	}
	if _, err := c.Statuses(t.Context(), iys.Email, users[3:]); !errors.Is(err, iys.ErrThrottled) || !strings.Contains(err.Error(), "H086") {
		t.Fatalf("İYS's limit = %v, want throttled H086", err)
	}
	n := srv.Calls("status")
	clk.Advance(30 * time.Minute)
	if _, err := c.Statuses(t.Context(), iys.Email, users[3:]); !errors.Is(err, iys.ErrThrottled) || srv.Calls("status") != n {
		t.Fatalf("within the pause = %v (%d → %d single queries)", err, n, srv.Calls("status"))
	}
	clk.Advance(31 * time.Minute)
	if _, err := c.Statuses(t.Context(), iys.Email, users[3:]); err != nil {
		t.Fatalf("after the pause: %v", err)
	}

	// The adapter's own budget stops before İYS's limit.
	budget := newClient(t, srv, clk, dir, func(c *official.Config) { c.StatusQueriesPerHour = 1 })
	clk.Advance(2 * time.Hour)
	n = srv.Calls("status")
	if _, err := budget.Statuses(t.Context(), iys.Email, users[2:]); !errors.Is(err, iys.ErrThrottled) || srv.Calls("status") != n+1 {
		t.Fatalf("over the adapter's budget = %v (%d → %d single queries)", err, n, srv.Calls("status"))
	}
}

type logSink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestOfficial_ErrorsCarryCodesAndTrace: a refused record wraps
// ErrRejected with İYS's code and the trace ID, never the address; the
// debug log has the trace IDs and no address, password or token; changes
// of the other recipient type are skipped and citizenName never surfaces.
func TestOfficial_ErrorsCarryCodesAndTrace(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	srv := iystest.New(t, iystest.WithClock(clk))
	dir := iyscontract.NewDirectory()
	ada := dir.Add("ada.yilmaz@ogrenci.example.test")
	var logs logSink
	c := newClient(t, srv, clk, dir, func(c *official.Config) {
		c.Log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	srv.Reject("ada.yilmaz@ogrenci.example.test")
	_, err := c.Submit(t.Context(), iys.Consent{Ref: "sub", UserID: ada, Type: iys.Email, Status: iys.Approved, Source: "HS_WEB",
		ConsentDate: clk.Now()})
	if !errors.Is(err, iys.ErrRejected) || !strings.Contains(err.Error(), "H464") || !strings.Contains(err.Error(), "trace "+srv.LastTrace()) ||
		strings.Contains(err.Error(), "ada.yilmaz") {
		t.Fatalf("refused record = %v", err)
	}
	srv.SetDown(true)
	_, err = c.Submit(t.Context(), iys.Consent{Ref: "sub", UserID: ada, Type: iys.Email, Status: iys.Approved, Source: "HS_WEB",
		ConsentDate: clk.Now()})
	if err == nil || errors.Is(err, iys.ErrRejected) || !strings.Contains(err.Error(), "HTTP 503") || !strings.Contains(err.Error(), "trace ") {
		t.Fatalf("outage = %v", err)
	}
	srv.SetDown(false)
	wrong := newClient(t, srv, clk, dir, func(c *official.Config) { c.Password = "wrong-iys-password-987" })
	_, err = wrong.Submit(t.Context(), iys.Consent{Ref: "sub", UserID: ada, Type: iys.Email, Status: iys.Approved, Source: "HS_WEB",
		ConsentDate: clk.Now()})
	if err == nil || errors.Is(err, iys.ErrRejected) || !strings.Contains(err.Error(), "sign-in refused") ||
		strings.Contains(err.Error(), "wrong-iys-password") {
		t.Fatalf("refused sign-in = %v, want a transient error without the password", err)
	}

	srv.Act(iystest.Action{Recipient: "ada.yilmaz@ogrenci.example.test", RecipientType: "TACIR"})
	srv.Act(iystest.Action{Recipient: "ada.yilmaz@ogrenci.example.test", Status: "ONAY", Source: "IYS_WEB_YENI", CitizenName: "Örnek Kişi"})
	clk.Advance(time.Hour + time.Minute)
	page, err := c.Changes(t.Context(), "")
	if err != nil || len(page.Changes) != 1 || page.Skipped != 1 || page.Changes[0].State.Source != "IYS_WEB_YENI" || page.More ||
		!page.Through.Equal(clk.Now().Add(-65*time.Minute)) {
		t.Fatalf("changes = %+v, %v", page, err)
	}
	out := logs.String()
	if !strings.Contains(out, `"trace_id":"`+srv.LastTrace()+`"`) {
		t.Fatalf("trace ID not logged: %s", out)
	}
	for _, secret := range []string{"ada.yilmaz", iystest.Password, "iys-access-", "iys-refresh-", "Örnek"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains %q: %s", secret, out)
		}
	}
}

func TestNew_Config(t *testing.T) {
	t.Parallel()
	base := official.Config{BaseURL: "https://api.iys.org.tr", Username: "u", Password: "p", IYSCode: "1", BrandCode: "2",
		Recipients: iyscontract.NewDirectory(), Clock: clock.Real()}
	c, err := official.New(base)
	if err != nil || c.Name() != "iys" {
		t.Fatalf("valid config = %v", err)
	}
	if f := c.Feed(); f.Name != "iys:1/2" || f.Window <= 6*24*time.Hour || f.Window >= 7*24*time.Hour {
		t.Fatalf("feed = %+v", f)
	}
	for _, mut := range []func(*official.Config){
		func(c *official.Config) { c.BaseURL = "ftp://iys" },
		func(c *official.Config) { c.BaseURL = "https://user:pw@iys" },
		func(c *official.Config) { c.BaseURL = "https://iys/?a=b" },
		func(c *official.Config) { c.Password = "" },
		func(c *official.Config) { c.IYSCode = "abc" },
		func(c *official.Config) { c.RecipientType = "KURUM" },
		func(c *official.Config) { c.Recipients = nil },
		func(c *official.Config) { c.StatusQueriesPerHour = 1001 },
		func(c *official.Config) { c.SignInsPerHour = -1 },
	} {
		c := base
		mut(&c)
		if _, err := official.New(c); err == nil {
			t.Errorf("New accepted %+v", c)
		}
	}
}
