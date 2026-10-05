//go:build integration

package notify_test

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var userSeq atomic.Int64

// insertUser writes a users row (the notify tables reference users;
// identity owns the table).
func insertUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	email := "notify" + strconv.FormatInt(userSeq.Add(1), 10) + "@example.test"
	_, err := d.Pool.Exec(t.Context(), `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, email)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// TestStore_Contract_Gorm runs the very body the in-memory store passes
// against PostgreSQL (T-98).
func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	world := func(t *testing.T) storeWorld {
		d := testutil.NewDB(t)
		return storeWorld{s: notify.NewGormStore(d.ORM), user: func(t *testing.T) uuid.UUID { return insertUser(t, d) }}
	}
	runStoreContract(t, world)
	runRetentionContract(t, world)
}

// TestSend_DedupeKeyAcrossWorkers_SingleDelivery: many workers (separate
// service instances, as separate processes would be) send the same message
// at once over one database; exactly one e-mail leaves and every caller
// sees the same delivery (dedupe_key UNIQUE + the conditional takeover).
func TestSend_DedupeKeyAcrossWorkers_SingleDelivery(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	uid := insertUser(t, d)
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	tr := notify.NewMemTransport()
	const workers = 16
	svcs := make([]notify.Service, workers)
	for i := range svcs {
		clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
		svcs[i] = notify.New(notify.Deps{
			Store: notify.NewGormStore(d.ORM), Tx: d.Tx, Transport: tr, Templates: tpl, Clock: clk, Log: obs.Discard(),
			FromAddress: "noreply@koro.test", Recipients: testIndexer{},
			Audit: audit.New(audit.Deps{Store: audit.NewGormStore(d.ORM), Tx: d.Tx, Clock: clk, Log: obs.Discard()}).Service,
		}).Service
	}
	m := testMessage("ticket:" + uuid.NewV7().String() + ":v1")
	m.UserID = &uid

	var wg sync.WaitGroup
	ids := make([]uuid.UUID, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() { ids[i], errs[i] = svcs[i].Send(obs.WithTaskID(t.Context(), "task-"+strconv.Itoa(i)), m) })
	}
	wg.Wait()
	for i := range workers {
		if errs[i] != nil && !errors.Is(errs[i], notify.ErrDeliveryInFlight) {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("worker %d saw delivery %s, want %s", i, ids[i], ids[0])
		}
	}
	if n := len(tr.Sent()); n != 1 {
		t.Fatalf("%d e-mails sent, want exactly 1", n)
	}
	// The retries the in-flight workers get from asynq find the final row.
	for i := range workers {
		id, err := svcs[i].Send(obs.WithTaskID(t.Context(), "retry-"+strconv.Itoa(i)), m)
		if err != nil || id != ids[0] {
			t.Fatalf("retry %d = (%s, %v)", i, id, err)
		}
	}
	got, err := notify.NewGormStore(d.ORM).Get(t.Context(), ids[0])
	if err != nil || got.Status != notify.StatusSent || got.Attempts != 1 || got.ProviderMessageID == "" {
		t.Fatalf("delivery = (%+v, %v), want one sent attempt", got, err)
	}
	if n := len(tr.Sent()); n != 1 {
		t.Fatalf("%d e-mails after the retries, want 1", n)
	}
}

// TestSend_SuppressionAndInApp_Gorm runs the suppression gate and the in-app
// channel over PostgreSQL (blind index, jsonb data, link_path CHECK).
func TestSend_SuppressionAndInApp_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	uid := insertUser(t, d)
	st := notify.NewGormStore(d.ORM)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	tr := notify.NewMemTransport()
	mod := notify.New(notify.Deps{
		Store: st, Tx: d.Tx, Transport: tr, Templates: loadTemplates(t, withPromo(baseTemplates(t))), Clock: clk,
		Log: obs.Discard(), FromAddress: "noreply@koro.test", Recipients: testIndexer{},
		Audit: audit.New(audit.Deps{Store: audit.NewGormStore(d.ORM), Tx: d.Tx, Clock: clk, Log: obs.Discard()}).Service,
	})
	ctx := obs.WithTaskID(t.Context(), "task-1")
	if _, err := st.Suppress(t.Context(), notify.Suppression{EmailIndex: index("ayse.yilmaz@ogrenci.edu.tr"),
		Reason: notify.SuppressHardBounce, Source: notify.SourceProviderWebhook, CreatedAt: clk.Now()}); err != nil {
		t.Fatal(err)
	}
	id, err := mod.Service.Send(ctx, testMessage("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get(t.Context(), id); got.Status != notify.StatusSuppressed || len(tr.Sent()) != 0 {
		t.Fatalf("delivery = %+v, want suppressed", got)
	}
	ev := uuid.NewV7()
	nid, err := mod.Service.Send(ctx, notify.Message{UserID: &uid, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
		Template: "notify.test_notice", Data: map[string]any{"event_id": ev}, LinkPath: "/events/" + ev.String(), DedupeKey: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListInApp(t.Context(), uid, true, nil, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != nid || rows[0].Data["event_id"] != ev.String() {
		t.Fatalf("inbox = (%+v, %v)", rows, err)
	}
	if res, err := mod.Privacy.Erase(t.Context(), uid); err != nil || res.Deleted != 2 {
		t.Fatalf("Erase() = (%+v, %v), want the delivery and the notification deleted", res, err)
	}
}
