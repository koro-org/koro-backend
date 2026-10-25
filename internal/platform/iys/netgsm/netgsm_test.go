package netgsm_test

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/iys"
	"koro/internal/platform/iys/iyscontract"
	"koro/internal/platform/iys/netgsm"
	"koro/internal/platform/iys/netgsm/netgsmtest"
)

var errStoreDown = errors.New("store unreachable (test)")

// rig is the adapter over the double, its store and the suite's directory.
type rig struct {
	srv    *netgsmtest.Server
	client *netgsm.Client
	store  *netgsm.MemStore
	dir    *iyscontract.Directory
	clk    *clock.Fake

	mu  sync.Mutex
	hit int // webhook deliveries the adapter took
}

// newRig wires the adapter and the double: the double's webhook goes to
// the adapter's Receive with the test token.
func newRig(t *testing.T, clk *clock.Fake, dir *iyscontract.Directory, srvOpts []netgsmtest.Option, mut ...func(*netgsm.Config)) *rig {
	t.Helper()
	r := &rig{store: netgsm.NewMemStore(), dir: dir, clk: clk}
	opts := append([]netgsmtest.Option{netgsmtest.WithClock(clk), netgsmtest.WithWebhook(func(ctx context.Context, body []byte) error {
		r.mu.Lock()
		r.hit++
		r.mu.Unlock()
		_, err := r.client.Receive(ctx, iys.Delivery{Token: netgsmtest.WebhookToken, ClientIP: "192.0.2.10", Body: body})
		return err
	})}, srvOpts...)
	r.srv = netgsmtest.New(t, opts...)
	c := netgsm.Config{BaseURL: r.srv.URL, Username: netgsmtest.Username, Password: netgsmtest.Password,
		IYSCode: netgsmtest.IYSCode, BrandCode: netgsmtest.BrandCode, WebhookToken: netgsmtest.WebhookToken,
		Recipients: dir, Store: r.store, Clock: clk, RequestsPerMinute: 10, StatusQueriesPerHour: 600,
		ResultWait: -1} // the double answers before /iys/add returns: one lookup is enough
	for _, m := range mut {
		m(&c)
	}
	client, err := netgsm.New(c)
	if err != nil {
		t.Fatal(err)
	}
	r.client = client
	return r
}

// TestNetgsm_Contract: the Netgsm adapter over the Netgsm double passes
// the contract every İYS adapter passes (iyscontract.Run). Its feed is
// pushed: a change is listed once it settled (netgsm.Settle); an
// unreachable store is the feed's outage.
func TestNetgsm_Contract(t *testing.T) {
	t.Parallel()
	iyscontract.Run(t, func(t *testing.T, env iyscontract.Env) iyscontract.Harness {
		r := newRig(t, env.Clock, env.Recipients, nil)
		return iyscontract.Harness{
			Adapter: r.client, Lag: netgsm.Settle,
			Act: func(t *testing.T, address string, typ iys.Type, st iys.Status) {
				r.srv.Act(t.Context(), netgsmtest.Action{Recipient: address, Type: string(typ), Status: string(st)})
			},
			Held: func(_ *testing.T, address string, typ iys.Type) (iys.State, bool) {
				status, at, source, ok := r.srv.Held(address, string(typ))
				return iys.State{Status: iys.Status(status), At: at, Source: source}, ok
			},
			SetDown: func(down bool) {
				r.srv.SetDown(down)
				if down {
					r.store.Fail(errStoreDown)
				} else {
					r.store.Fail(nil)
				}
			},
		}
	})
}

func approval(user uuid.UUID, at time.Time) iys.Consent {
	return iys.Consent{Ref: uuid.NewV7().String(), UserID: user, Type: iys.Email, Status: iys.Approved, Source: "HS_WEB", ConsentDate: at}
}

// TestNetgsm_SubmitWaitsForTheWebhookAnswer: /iys/add only queues the
// upload; Submit succeeds once İYS's answer arrived by webhook, does not
// send again while the answer is on its way, sends again after
// ResendAfter, and sends again after a transient İYS failure.
func TestNetgsm_SubmitWaitsForTheWebhookAnswer(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	dir := iyscontract.NewDirectory()
	ada := dir.Add("ada.yilmaz@ogrenci.example.test")
	r := newRig(t, clk, dir, nil, func(c *netgsm.Config) { c.ResendAfter = 30 * time.Minute })
	ctx := t.Context()

	r.srv.HoldResults(true)
	c := approval(ada, clk.Now())
	if _, err := r.client.Submit(ctx, c); err == nil || errors.Is(err, iys.ErrRejected) || !strings.Contains(err.Error(), "has not arrived") {
		t.Fatalf("Submit before İYS's answer = %v, want a transient wait", err)
	}
	clk.Advance(time.Minute)
	if _, err := r.client.Submit(ctx, c); err == nil {
		t.Fatal("retry before the answer succeeded")
	}
	if n := r.srv.Calls("add"); n != 1 {
		t.Fatalf("%d uploads, want one while the answer is on its way", n)
	}
	r.srv.Flush(ctx)
	rc, err := r.client.Submit(ctx, c)
	if err != nil || rc.AlreadyHeld || len(rc.TransactionID) != 64 {
		t.Fatalf("Submit after İYS's answer = %+v, %v; want İYS's transaction ID", rc, err)
	}
	if n := r.srv.Calls("add"); n != 1 {
		t.Fatalf("%d uploads after the answer, want 1", n)
	}
	if up := r.srv.Uploads(); len(up) != 1 || up[0].RefIn != netgsm.RefInHeader || up[0].RefID != c.Ref ||
		up[0].Recipient != "ada.yilmaz@ogrenci.example.test" || up[0].ConsentDate != clk.Now().Add(-time.Minute).In(clock.Istanbul()).Format("2006-01-02 15:04:05") {
		t.Fatalf("uploads = %+v", up)
	}

	// No answer for ResendAfter: the upload is sent again.
	r.srv.HoldResults(true)
	c2 := approval(ada, clk.Now())
	_, _ = r.client.Submit(ctx, c2)
	clk.Advance(31 * time.Minute)
	_, _ = r.client.Submit(ctx, c2)
	if n := r.srv.Calls("add"); n != 3 {
		t.Fatalf("%d uploads, want a resend after ResendAfter", n)
	}
	r.srv.Flush(ctx)
	if _, err := r.client.Submit(ctx, c2); err != nil {
		t.Fatalf("after both answers: %v", err)
	}

	// A transient İYS failure (H103) is sent again at the next attempt.
	r.srv.HoldResults(false)
	r.srv.FailNext("H103")
	c3 := approval(ada, clk.Now())
	if _, err := r.client.Submit(ctx, c3); err == nil || errors.Is(err, iys.ErrRejected) {
		t.Fatalf("Submit answered H103 = %v, want transient", err)
	}
	if _, err := r.client.Submit(ctx, c3); err != nil {
		t.Fatalf("Submit after a transient İYS failure = %v, want sent again and taken", err)
	}
	if n := r.srv.Calls("add"); n != 5 {
		t.Fatalf("%d uploads, want a resend after H103", n)
	}
}

// TestNetgsm_SubmitMatchesAnswersWithoutReference: when Netgsm drops the
// refid (no user_refcode), the answer is matched by the user, type,
// status and consent date; RefInItem sends the reference in the item.
func TestNetgsm_SubmitMatchesAnswersWithoutReference(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	dir := iyscontract.NewDirectory()
	ada := dir.Add("ada.yilmaz@ogrenci.example.test")
	r := newRig(t, clk, dir, []netgsmtest.Option{netgsmtest.WithoutRefEcho()}, func(c *netgsm.Config) { c.RefIn = netgsm.RefInItem })
	c := approval(ada, clk.Now().Add(-10*time.Minute))
	if rc, err := r.client.Submit(t.Context(), c); err != nil || rc.TransactionID == "" {
		t.Fatalf("Submit = %+v, %v", rc, err)
	}
	if up := r.srv.Uploads(); len(up) != 1 || up[0].RefIn != netgsm.RefInItem || up[0].RefID != c.Ref {
		t.Fatalf("uploads = %+v, want the reference in the item", up)
	}
	ev := r.store.All("netgsm:" + netgsmtest.IYSCode + "/" + netgsmtest.BrandCode)
	if len(ev) != 1 || ev[0].Ref != "" || ev[0].UserID != ada || ev[0].Kind != netgsm.KindResult {
		t.Fatalf("stored answers = %+v", ev)
	}
}

// TestNetgsm_ErrorsAreClassified: Netgsm's codes and İYS's answers map to
// the port errors without repeating the address or a credential; a spent
// budget fails before a call.
func TestNetgsm_ErrorsAreClassified(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	dir := iyscontract.NewDirectory()
	ada := dir.Add("ada.yilmaz@ogrenci.example.test")
	bad := dir.Add("not-an-address") // Netgsm refuses its form (erroritem)

	wrong := newRig(t, clk, dir, nil, func(c *netgsm.Config) { c.Password = "wrong-password-of-the-test" })
	_, err := wrong.client.Submit(t.Context(), approval(ada, clk.Now()))
	if !errors.Is(err, iys.ErrRejected) || !strings.Contains(err.Error(), "code 30") {
		t.Fatalf("wrong credentials = %v, want ErrRejected code 30", err)
	}
	brand := newRig(t, clk, dir, nil, func(c *netgsm.Config) { c.BrandCode = "999999" })
	if _, err := brand.client.Submit(t.Context(), approval(ada, clk.Now())); !errors.Is(err, iys.ErrRejected) || !strings.Contains(err.Error(), "code 60") {
		t.Fatalf("wrong brand = %v, want ErrRejected code 60", err)
	}

	// An approval costs two calls (the upload and its confirmation, Q-16).
	r := newRig(t, clk, dir, []netgsmtest.Option{netgsmtest.WithRateLimit(3)}, func(c *netgsm.Config) { c.RequestsPerMinute = 4 })
	ctx := t.Context()
	var errs []error
	_, err = r.client.Submit(ctx, approval(bad, clk.Now()))
	if !errors.Is(err, iys.ErrRejected) || !strings.Contains(err.Error(), "erroritem") {
		t.Fatalf("address of the wrong form = %v, want ErrRejected (erroritem)", err)
	}
	errs = append(errs, err)
	if _, err := r.client.Submit(ctx, approval(ada, clk.Now())); err != nil {
		t.Fatal(err)
	}
	_, err = r.client.Submit(ctx, approval(ada, clk.Now().Add(time.Second)))
	if !errors.Is(err, iys.ErrThrottled) || !strings.Contains(err.Error(), "code 101") {
		t.Fatalf("Netgsm's rate limit = %v, want ErrThrottled code 101", err)
	}
	errs = append(errs, err)
	calls := r.srv.Calls("add")
	_, err = r.client.Submit(ctx, approval(ada, clk.Now().Add(2*time.Second)))
	if !errors.Is(err, iys.ErrThrottled) || r.srv.Calls("add") != calls {
		t.Fatalf("over the per-minute budget = %v (%d → %d calls), want throttled without a call", err, calls, r.srv.Calls("add"))
	}
	errs = append(errs, err)
	for _, err := range append(errs, wrongErr(t, wrong, ada, clk)) {
		for _, secret := range []string{"ada.yilmaz", "not-an-address", netgsmtest.Password, "wrong-password-of-the-test"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error repeats %q: %v", secret, err)
			}
		}
	}
}

func wrongErr(t *testing.T, r *rig, user uuid.UUID, clk *clock.Fake) error {
	t.Helper()
	_, err := r.client.Submit(t.Context(), approval(user, clk.Now().Add(3*time.Second)))
	return err
}

// TestNetgsm_WebhookAuthenticatesAndKeepsLeastData: a delivery proves
// itself by the URL's secret and (when configured) its source address;
// items of another brand are dropped, a redelivery stores nothing, and
// no address, name or Netgsm text is ever stored.
func TestNetgsm_WebhookAuthenticatesAndKeepsLeastData(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	dir := iyscontract.NewDirectory()
	ada := dir.Add("ada.yilmaz@ogrenci.example.test")
	allowed := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	r := newRig(t, clk, dir, nil, func(c *netgsm.Config) { c.WebhookIPs = allowed })
	ctx := t.Context()
	body := []byte(`[{"iyscode": 700123, "brandcode": 700124, "submitid": "", "resultstatus": "success", "errcode": "",
		"errmsg": "", "creationdate": "2026-10-21 12:00:05", "user_refcode": "", "transactionid": "ab12",
		"type": "EPOSTA", "source": "IYS_WEB", "recipient": "Ada.Yilmaz@ogrenci.example.test", "status": "RET",
		"consentDate": "2026-10-21 12:00:00", "recipientType": "BIREYSEL", "retailerCode": "0", "citizenName": "Ada Yılmaz"},
		{"iyscode": 700123, "brandcode": 999999, "type": "EPOSTA", "source": "IYS_WEB", "recipient": "ada.yilmaz@ogrenci.example.test",
		"status": "RET", "consentDate": "2026-10-21 12:00:00", "recipientType": "BIREYSEL"},
		{"iyscode": "700123", "brandcode": "700124", "type": "EPOSTA", "source": "IYS_MOBIL", "recipient": "stranger@elsewhere.example.test",
		"status": "RET", "consentdate": "2026-10-21 12:01:00", "recipienttype": "BIREYSEL"}]`)

	for _, d := range []iys.Delivery{
		{Token: "", ClientIP: "192.0.2.10", Body: body},
		{Token: netgsmtest.WebhookToken + "x", ClientIP: "192.0.2.10", Body: body},
		{Token: netgsmtest.WebhookToken, ClientIP: "198.51.100.7", Body: body},
		{Token: netgsmtest.WebhookToken, ClientIP: "", Body: body},
	} {
		if _, err := r.client.Receive(ctx, d); !errors.Is(err, iys.ErrUnauthenticated) {
			t.Fatalf("Receive(token %d chars, ip %q) = %v, want ErrUnauthenticated", len(d.Token), d.ClientIP, err)
		}
	}
	feed := r.client.Feed().Name
	if n := len(r.store.All(feed)); n != 0 {
		t.Fatalf("%d events stored from unauthenticated deliveries", n)
	}
	for _, junk := range []string{"", "not json", `{"a":`, `"text"`, "[1, 2]"} {
		if _, err := r.client.Receive(ctx, iys.Delivery{Token: netgsmtest.WebhookToken, ClientIP: "192.0.2.10", Body: []byte(junk)}); !errors.Is(err, iys.ErrRejected) {
			t.Fatalf("Receive(%q) = %v, want ErrRejected", junk, err)
		}
	}
	good := iys.Delivery{Token: netgsmtest.WebhookToken, ClientIP: "::ffff:192.0.2.10", Body: body}
	if n, err := r.client.Receive(ctx, good); err != nil || n != 2 {
		t.Fatalf("Receive = %d, %v; want ada's refusal and the stranger's marker", n, err)
	}
	if n, err := r.client.Receive(ctx, good); err != nil || n != 0 {
		t.Fatalf("redelivery = %d, %v; want nothing new", n, err)
	}
	evs := r.store.All(feed)
	if len(evs) != 2 || evs[0].UserID != ada || evs[0].Status != iys.Refused || evs[0].Source != "IYS_WEB" ||
		evs[0].Kind != netgsm.KindChange || evs[1].UserID != uuid.Nil() || evs[1].Source != "IYS_MOBIL" {
		t.Fatalf("stored = %+v", evs)
	}
	for _, e := range evs {
		for _, s := range []string{e.Ref, e.SubmitID, e.Source, e.TransactionID, e.Key, e.ErrCode, e.Result} {
			if strings.ContainsAny(s, "@ ") || strings.Contains(s, "Ada") {
				t.Fatalf("stored event carries personal text: %+v", e)
			}
		}
	}
	// One item (the PHP package's example) is accepted; more than
	// MaxItems is refused.
	one := []byte(`{"iyscode": 700123, "brandcode": 700124, "type": "EPOSTA", "source": "IYS_3338",
		"recipient": "ada.yilmaz@ogrenci.example.test", "status": "ONAY", "consentDate": "2026-10-21 13:00:00",
		"recipientType": "BIREYSEL", "resultstatus": "success", "transactionid": "cd34"}`)
	if n, err := r.client.Receive(ctx, iys.Delivery{Token: netgsmtest.WebhookToken, ClientIP: "192.0.2.10", Body: one}); err != nil || n != 1 {
		t.Fatalf("single item = %d, %v", n, err)
	}
	many := []byte("[" + strings.Repeat(`{},`, netgsm.MaxItems) + `{}]`)
	if _, err := r.client.Receive(ctx, iys.Delivery{Token: netgsmtest.WebhookToken, ClientIP: "192.0.2.10", Body: many}); !errors.Is(err, iys.ErrRejected) {
		t.Fatalf("%d items = %v, want ErrRejected", netgsm.MaxItems+1, err)
	}
	// An unresolvable lookup is transient: Netgsm's retry brings it again.
	dir2 := failingDirectory{Directory: dir}
	r2 := newRig(t, clk, dir, nil, func(c *netgsm.Config) { c.Recipients = dir2 })
	if _, err := r2.client.Receive(ctx, good); err == nil || errors.Is(err, iys.ErrRejected) || errors.Is(err, iys.ErrUnauthenticated) {
		t.Fatalf("Receive with identity down = %v, want a transient error", err)
	}
}

type failingDirectory struct{ *iyscontract.Directory }

func (failingDirectory) User(context.Context, iys.Type, string) (uuid.UUID, error) {
	return uuid.Nil(), errors.New("identity unreachable (test)")
}

// TestNetgsm_RecheckAddsMissedRefusals: a refusal whose webhook was lost
// is found by the backup check and listed by the feed; one the webhook
// delivered is not added twice; Koro's own refusal is left out.
func TestNetgsm_RecheckAddsMissedRefusals(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	dir := iyscontract.NewDirectory()
	ada, bea := dir.Add("ada.yilmaz@ogrenci.example.test"), dir.Add("bea.kaya@ogrenci.example.test")
	cem, gone := dir.Add("cem.demir@ogrenci.example.test"), uuid.NewV7()
	r := newRig(t, clk, dir, nil)
	ctx := t.Context()
	for _, u := range []uuid.UUID{ada, bea, cem} {
		if _, err := r.client.Submit(ctx, approval(u, clk.Now())); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(time.Minute)
	r.srv.DropWebhooks(true)
	r.srv.Act(ctx, netgsmtest.Action{Recipient: "ada.yilmaz@ogrenci.example.test", Source: "IYS_WEB"}) // lost
	r.srv.DropWebhooks(false)
	r.srv.Act(ctx, netgsmtest.Action{Recipient: "bea.kaya@ogrenci.example.test", Source: "IYS_MOBIL"}) // delivered
	if _, err := r.client.Submit(ctx, iys.Consent{Ref: uuid.NewV7().String(), UserID: cem, Type: iys.Email, Status: iys.Refused,
		Source: "HS_EPOSTA", ConsentDate: clk.Now()}); err != nil {
		t.Fatal(err)
	}
	res, err := r.client.Recheck(ctx, iys.Email, []uuid.UUID{ada, bea, cem, gone})
	if err != nil || res.Checked != 3 || res.Refusals != 1 {
		t.Fatalf("Recheck = %+v, %v; want ada's lost refusal only", res, err)
	}
	if again, err := r.client.Recheck(ctx, iys.Email, []uuid.UUID{ada}); err != nil || again.Refusals != 0 {
		t.Fatalf("second Recheck = %+v, %v; want nothing new", again, err)
	}
	clk.Advance(netgsm.Settle + time.Minute)
	page, err := r.client.Changes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var users []uuid.UUID
	for _, c := range page.Changes {
		users = append(users, c.UserID)
	}
	if !slices.Contains(users, ada) || !slices.Contains(users, bea) || len(users) != 2 {
		t.Fatalf("changes of %v, want ada (recheck) and bea (webhook)", users)
	}

	// The job reads the due users one by one, RecheckSpacing apart.
	paced := newRig(t, clk, dir, nil, func(c *netgsm.Config) { c.RecheckSpacing = time.Millisecond })
	for _, u := range []uuid.UUID{ada, bea, cem} {
		if _, err := paced.client.Submit(ctx, approval(u, clk.Now())); err != nil {
			t.Fatal(err)
		}
	}
	searches := paced.srv.Calls("search")
	pacedJob := paced.client.RecheckJob(iys.Email, 600, func(context.Context) ([]uuid.UUID, error) { return []uuid.UUID{ada, bea, cem}, nil })
	if err := pacedJob.Handle(ctx); err != nil || paced.srv.Calls("search") != searches+3 {
		t.Fatalf("job = %v, %d searches; want the three users read", err, paced.srv.Calls("search")-searches)
	}

	// A spent single-query budget stops the check (the job ends quietly).
	budget := newRig(t, clk, dir, nil, func(c *netgsm.Config) { c.StatusQueriesPerHour = 1; c.RecheckSpacing = time.Millisecond })
	if res, err := budget.client.Recheck(ctx, iys.Email, []uuid.UUID{ada, bea}); !errors.Is(err, iys.ErrThrottled) || res.Checked != 1 {
		t.Fatalf("Recheck over the budget = %+v, %v", res, err)
	}
	job := budget.client.RecheckJob(iys.Email, 10, func(context.Context) ([]uuid.UUID, error) { return []uuid.UUID{ada, bea}, nil })
	if job.Name != netgsm.RecheckJobName || job.Cron == "" || job.Unique <= 0 {
		t.Fatalf("job = %+v", job)
	}
	if err := job.Handle(ctx); err != nil {
		t.Fatalf("job with the budget spent = %v, want a quiet stop", err)
	}
}

// TestNetgsm_RecheckDueRotates: within one rotation every user is due in
// exactly one hour, at most perHour an hour.
func TestNetgsm_RecheckDueRotates(t *testing.T) {
	t.Parallel()
	users := make([]uuid.UUID, 250)
	for i := range users {
		users[i] = uuid.NewV7()
	}
	const per = 100
	slots := (len(users) + per - 1) / per
	start := time.Unix(0, 0).UTC().Add(time.Duration(slots*300000) * netgsm.RecheckPeriod) // a rotation boundary
	seen := map[uuid.UUID]int{}
	for h := range slots {
		due := netgsm.RecheckDue(users, start.Add(time.Duration(h)*netgsm.RecheckPeriod), netgsm.RecheckPeriod, per)
		if len(due) > per {
			t.Fatalf("hour %d: %d due, at most %d", h, len(due), per)
		}
		for _, u := range due {
			seen[u]++
		}
	}
	capped := 0
	for _, u := range users {
		switch seen[u] {
		case 0:
			capped++
		case 1:
		default:
			t.Fatalf("user due %d times in one rotation", seen[u])
		}
	}
	if capped > len(users)/5 {
		t.Fatalf("%d of %d users left out by the cap", capped, len(users))
	}
	if got := netgsm.RecheckDue(users, start, netgsm.RecheckPeriod, 0); got != nil {
		t.Fatalf("perHour 0 = %v, want off", got)
	}
}

// TestNetgsm_StatusesWithoutDates: a search answer without dates and
// source (the documentation's sample) gives the status only.
func TestNetgsm_StatusesWithoutDates(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	dir := iyscontract.NewDirectory()
	ada := dir.Add("ada.yilmaz@ogrenci.example.test")
	r := newRig(t, clk, dir, []netgsmtest.Option{netgsmtest.WithoutDates()})
	if _, err := r.client.Submit(t.Context(), approval(ada, clk.Now())); err != nil {
		t.Fatal(err) // confirmed by the status alone
	}
	searches := r.srv.Calls("search")
	got, err := r.client.Statuses(t.Context(), iys.Email, []uuid.UUID{ada, ada})
	if err != nil || got[ada].Status != iys.Approved || !got[ada].At.IsZero() || got[ada].Source != "" || r.srv.Calls("search") != searches+1 {
		t.Fatalf("Statuses = %+v, %v (%d searches)", got, err, r.srv.Calls("search")-searches)
	}
}

// TestNetgsm_ConfigValidation: New refuses incomplete settings.
func TestNetgsm_ConfigValidation(t *testing.T) {
	t.Parallel()
	ok := netgsm.Config{Username: netgsmtest.Username, Password: netgsmtest.Password, IYSCode: netgsmtest.IYSCode,
		BrandCode: netgsmtest.BrandCode, WebhookToken: netgsmtest.WebhookToken, Recipients: iyscontract.NewDirectory(),
		Store: netgsm.NewMemStore(), Clock: clock.NewFake(iyscontract.Start)}
	if c, err := netgsm.New(ok); err != nil || c.Name() != "netgsm" || c.Feed().Name != "netgsm:700123/700124" {
		t.Fatalf("New(defaults) = %v, %v", c, err)
	}
	for name, mut := range map[string]func(*netgsm.Config){
		"base url":       func(c *netgsm.Config) { c.BaseURL = "ftp://api.netgsm.example.test" },
		"credentials":    func(c *netgsm.Config) { c.Password = "" },
		"brand":          func(c *netgsm.Config) { c.BrandCode = "68xxxx" },
		"iys code":       func(c *netgsm.Config) { c.IYSCode = "" },
		"short token":    func(c *netgsm.Config) { c.WebhookToken = "short" },
		"token chars":    func(c *netgsm.Config) { c.WebhookToken = strings.Repeat("x", 31) + "/" },
		"store":          func(c *netgsm.Config) { c.Store = nil },
		"rate":           func(c *netgsm.Config) { c.RequestsPerMinute = 11 },
		"recipient type": func(c *netgsm.Config) { c.RecipientType = "KURUMSAL" },
		"refid":          func(c *netgsm.Config) { c.RefIn = "query" },
	} {
		c := ok
		mut(&c)
		if _, err := netgsm.New(c); err == nil {
			t.Errorf("%s: New accepted %+v", name, c)
		}
	}
}

// TestNetgsm_SubmitWaitsUpToResultWait: right after the upload, Submit
// waits (real time, up to ResultWait) for the answer the webhook brings.
func TestNetgsm_SubmitWaitsUpToResultWait(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	dir := iyscontract.NewDirectory()
	ada := dir.Add("ada.yilmaz@ogrenci.example.test")
	r := newRig(t, clk, dir, nil, func(c *netgsm.Config) { c.ResultWait = 5 * time.Second })
	r.srv.HoldResults(true)
	ctx := t.Context()
	timer := time.AfterFunc(150*time.Millisecond, func() { r.srv.Flush(ctx) })
	defer timer.Stop()
	if rc, err := r.client.Submit(ctx, approval(ada, clk.Now())); err != nil || rc.TransactionID == "" {
		t.Fatalf("Submit with the answer arriving during the wait = %+v, %v", rc, err)
	}
}

// TestNetgsm_PurgeFollowsTheRetentionPolicy: the daily job deletes the
// items after the policy's period, never before the feed's window.
func TestNetgsm_PurgeFollowsTheRetentionPolicy(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		period    time.Duration
		keptAfter time.Duration // an item this old survives
		goneAfter time.Duration // an item this old is purged
	}{
		"default window": {0, 6 * 24 * time.Hour, 8 * 24 * time.Hour},
		"lengthened":     {30 * 24 * time.Hour, 20 * 24 * time.Hour, 31 * 24 * time.Hour},
		"too short":      {24 * time.Hour, 6 * 24 * time.Hour, 8 * 24 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			clk := clock.NewFake(iyscontract.Start)
			store := netgsm.NewMemStore()
			const feed = "netgsm:700123/700124"
			now := clk.Now()
			if _, err := store.Append(t.Context(), feed, []netgsm.Event{
				{Key: key(90), Kind: netgsm.KindChange, Type: iys.Email, Status: iys.Refused, ConsentAt: now, ReceivedAt: now.Add(-tc.keptAfter)},
				{Key: key(91), Kind: netgsm.KindChange, Type: iys.Email, Status: iys.Refused, ConsentAt: now, ReceivedAt: now.Add(-tc.goneAfter)},
			}); err != nil {
				t.Fatal(err)
			}
			d := netgsm.PurgeDeps{Store: store, Clock: clk}
			if tc.period > 0 {
				d.Period = func(context.Context) (time.Duration, error) { return tc.period, nil }
			}
			job := netgsm.PurgeJob(d)
			if job.Name != netgsm.PurgeJobName || job.Unique <= 0 {
				t.Fatalf("job = %+v", job)
			}
			if err := job.Handle(t.Context()); err != nil {
				t.Fatal(err)
			}
			if left := store.All(feed); len(left) != 1 || left[0].Key != key(90) {
				t.Fatalf("left after the purge = %+v", left)
			}
		})
	}
}

// forgedSuccess is a webhook item that claims İYS registered the upload
// ref of user's address (Q-16: the URL's secret is the only proof).
func forgedSuccess(address, ref string, at time.Time) []byte {
	return []byte(`[{"iyscode": 700123, "brandcode": 700124, "submitid": "00000000-0000-4000-8000-999999999999",
		"resultstatus": "success", "errcode": "", "creationdate": "` + at.In(clock.Istanbul()).Format("2006-01-02 15:04:05") + `",
		"user_refcode": "` + ref + `", "transactionid": "ff00ff00", "type": "EPOSTA", "source": "HS_WEB", "recipient": "` + address + `",
		"status": "ONAY", "consentDate": "` + at.In(clock.Istanbul()).Format("2006-01-02 15:04:05") + `", "recipientType": "BIREYSEL"}]`)
}

// TestNetgsm_SuccessAnswerNeedsIYSRecord (Q-16, BE-114): a success answer
// to an approval counts only once İYS's own record (/iys/search) holds
// the approval. A forged success over a lost failure is not taken: Submit
// fails transiently, does not upload again before ResendAfter, and then
// uploads again and gets İYS's real answer. A search that cannot be made
// (spent budget) is transient and uploads nothing; a success answer to a
// refusal needs no search.
func TestNetgsm_SuccessAnswerNeedsIYSRecord(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(iyscontract.Start)
	dir := iyscontract.NewDirectory()
	const address = "ada.yilmaz@ogrenci.example.test"
	ada := dir.Add(address)
	r := newRig(t, clk, dir, nil, func(c *netgsm.Config) { c.ResendAfter = 30 * time.Minute })
	ctx := t.Context()

	// İYS refuses a week-old consent (H467): the real answer is lost and
	// a forged success arrives with the right token and reference.
	c := approval(ada, clk.Now().Add(-7*24*time.Hour))
	r.srv.DropWebhooks(true)
	if _, err := r.client.Submit(ctx, c); err == nil || !strings.Contains(err.Error(), "has not arrived") {
		t.Fatalf("Submit without an answer = %v", err)
	}
	if n, err := r.client.Receive(ctx, iys.Delivery{Token: netgsmtest.WebhookToken, ClientIP: "192.0.2.10",
		Body: forgedSuccess(address, c.Ref, c.ConsentDate)}); err != nil || n != 1 {
		t.Fatalf("forged delivery = %d, %v (stored like any authenticated one)", n, err)
	}
	searches := r.srv.Calls("search")
	rc, err := r.client.Submit(ctx, c)
	if err == nil || rc.TransactionID != "" || errors.Is(err, iys.ErrRejected) || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("Submit over the forged success = %+v, %v; want a transient, unconfirmed answer", rc, err)
	}
	if got := r.srv.Calls("search"); got != searches+1 {
		t.Fatalf("%d searches, want İYS's record read once", got-searches)
	}
	if n := r.srv.Calls("add"); n != 1 {
		t.Fatalf("%d uploads, want no resend before ResendAfter", n)
	}
	// After ResendAfter the upload goes again and İYS's real answer decides.
	r.srv.DropWebhooks(false)
	clk.Advance(31 * time.Minute)
	if _, err := r.client.Submit(ctx, c); !errors.Is(err, iys.ErrTooLate) || !errors.Is(err, iys.ErrRejected) {
		t.Fatalf("Submit after ResendAfter = %v, want İYS's H467", err)
	}
	if n := r.srv.Calls("add"); n != 2 {
		t.Fatalf("%d uploads, want the resend", n)
	}

	// A success answer to a refusal is taken without a search (the safe
	// direction).
	if _, err := r.client.Submit(ctx, approval(ada, clk.Now())); err != nil {
		t.Fatal(err)
	}
	searches = r.srv.Calls("search")
	refusal := iys.Consent{Ref: uuid.NewV7().String(), UserID: ada, Type: iys.Email, Status: iys.Refused, Source: "HS_EPOSTA",
		ConsentDate: clk.Now().Add(time.Second)}
	if rc, err := r.client.Submit(ctx, refusal); err != nil || rc.TransactionID == "" || r.srv.Calls("search") != searches {
		t.Fatalf("refusal = %+v, %v (%d searches)", rc, err, r.srv.Calls("search")-searches)
	}

	// The confirmation cannot be made (the single-query budget is spent):
	// transient, and the upload is not sent again.
	tight := newRig(t, clk, dir, nil, func(c *netgsm.Config) { c.StatusQueriesPerHour = 1 })
	if _, err := tight.client.Submit(ctx, approval(ada, clk.Now())); err != nil {
		t.Fatal(err)
	}
	next := approval(ada, clk.Now().Add(2*time.Second))
	for range 2 {
		if _, err := tight.client.Submit(ctx, next); !errors.Is(err, iys.ErrThrottled) || errors.Is(err, iys.ErrRejected) {
			t.Fatalf("Submit with the budget spent = %v, want throttled", err)
		}
	}
	if n := tight.srv.Calls("add"); n != 2 {
		t.Fatalf("%d uploads, want one per consent", n)
	}
}
