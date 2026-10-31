//go:build integration

package consent_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var userSeq atomic.Int64

// insertUser writes a users row (consents.user_id and
// notice_versions.created_by point to users; identity owns the table).
func insertUser(t *testing.T, d *db.DB, id uuid.UUID) uuid.UUID {
	t.Helper()
	email := "consent" + strconv.FormatInt(userSeq.Add(1), 10) + "@example.test"
	_, err := d.Pool.Exec(t.Context(), `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, email)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) storeWorld {
		d := testutil.NewDB(t)
		return storeWorld{s: consent.NewGormStore(d.ORM), tx: d.Tx, user: func(t *testing.T) uuid.UUID { return insertUser(t, d, uuid.NewV7()) }}
	})
}

// pgEnv is the consent module over PostgreSQL: GORM stores of consent,
// outbox, audit and idempotency, real transactions.
type pgEnv struct {
	d     *db.DB
	clk   *clock.Fake
	store consent.Store
	iys   *consent.FakeIYS
	mod   consent.Module
	svc   consent.Service
}

func newPGEnv(t *testing.T, opts ...func(*consent.Deps)) *pgEnv {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	e := &pgEnv{d: d, clk: clk, store: consent.NewGormStore(d.ORM), iys: &consent.FakeIYS{}}
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: outbox.NewRegistry(), Queue: queue.NewMem(), Clock: clk, Log: obs.Discard()})
	deps := consent.Deps{
		Store: e.store, Tx: d.Tx, Outbox: ob, Clock: clk, Log: obs.Discard(),
		Audit:       audit.New(audit.Deps{Store: audit.NewGormStore(d.ORM), Tx: d.Tx, Clock: clk, Log: obs.Discard()}).Service,
		Idempotency: idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()}),
		Reauth:      fakeReauth{}, IYS: e.iys,
	}
	for _, o := range opts {
		o(&deps)
	}
	mod, err := consent.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	insertUser(t, d, authztest.ID("dpo")) // notice_versions.created_by
	return e
}

func (e *pgEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.d.Pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *pgEnv) consentText(t *testing.T, purpose consent.Purpose) consent.Notice {
	t.Helper()
	dpo := actor(t, "dpo")
	n, err := e.svc.CreateNotice(t.Context(), dpo, consent.NoticeDraft{Kind: consent.KindConsentText, Purpose: &purpose,
		Audience: consent.AudienceAll, Locale: consent.LocaleTR, Version: "2026.10.01", Title: "Rıza", BodyMD: "Rıza metni " + string(purpose) + "\n"}, proof)
	if err != nil {
		t.Fatal(err)
	}
	if n, err = e.svc.PublishNotice(t.Context(), dpo, n.ID, proof); err != nil {
		t.Fatal(err)
	}
	return n
}

var errBoom = errors.New("caller failed after recording")

// TestRecord_InCallerTx_RollbackRemovesConsent (01 §6.3): Record joins the
// caller's transaction; when the caller rolls back, the consent, its İYS
// submission, its outbox event and its audit row disappear together.
func TestRecord_InCallerTx_RollbackRemovesConsent(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	user := insertUser(t, e.d, uuid.NewV7())
	text := e.consentText(t, consent.PurposeCommercialEmail)
	baseOutbox := e.count(t, `SELECT count(*) FROM outbox WHERE topic = 'consent.recorded'`)
	baseAudit := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'consent.record.create'`)

	var id uuid.UUID
	err := e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if id, err = e.svc.Record(ctx, grant(user, text, consent.ChannelWeb)); err != nil {
			return err
		}
		if on, err := e.svc.HasActive(ctx, user, consent.PurposeCommercialEmail, nil); err != nil || !on {
			t.Fatalf("inside the transaction HasActive = %v, %v", on, err)
		}
		return errBoom // e.g. the registration failed after the consent step
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("WithTx = %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM consents WHERE user_id = $1`, user); n != 0 {
		t.Fatalf("consents after rollback = %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM iys_submissions WHERE consent_id = $1`, id); n != 0 {
		t.Fatalf("iys_submissions after rollback = %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM outbox WHERE topic = 'consent.recorded'`); n != baseOutbox {
		t.Fatalf("outbox rows after rollback = %d, want %d", n, baseOutbox)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'consent.record.create'`); n != baseAudit {
		t.Fatalf("audit rows after rollback = %d, want %d", n, baseAudit)
	}
	if on, _ := e.svc.HasActive(ctx, user, consent.PurposeCommercialEmail, nil); on {
		t.Fatal("a rolled back consent is in force")
	}

	// The same call committed by the caller writes all four.
	err = e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		id, err = e.svc.Record(ctx, grant(user, text, consent.ChannelWeb))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM consents WHERE id = $1`, id) != 1 ||
		e.count(t, `SELECT count(*) FROM iys_submissions WHERE consent_id = $1 AND status = 'pending'`, id) != 1 ||
		e.count(t, `SELECT count(*) FROM outbox WHERE topic = 'consent.recorded' AND aggregate_id = $1`, id) != 1 ||
		e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'consent.record.create' AND resource_id = $1`, id) != 1 {
		t.Fatal("a committed record lacks its consent, submission, event or audit row")
	}
}

// TestPublishedNotice_ImmutableInDatabase (02 §4.2): the service publishes
// and retires through the only change notice_versions_guard allows; any
// other change of a published row and every delete fail in the database.
func TestPublishedNotice_ImmutableInDatabase(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeContactInfoShare)
	if _, err := e.d.Pool.Exec(ctx, `UPDATE notice_versions SET body_md = 'changed' WHERE id = $1`, text.ID); err == nil {
		t.Fatal("a published body was changed")
	}
	if _, err := e.d.Pool.Exec(ctx, `UPDATE consents SET action = 'withdrawn'`); err == nil {
		t.Fatal("app_rw may update consents")
	}
	retired, err := e.svc.RetireNotice(ctx, actor(t, "dpo"), text.ID, proof)
	if err != nil || retired.Status() != consent.StatusRetired || retired.BodyMD != text.BodyMD {
		t.Fatalf("RetireNotice = %+v, %v", retired, err)
	}
	got, err := e.svc.GetNotice(ctx, actor(t, "student"), text.ID)
	if err != nil || got.Status() != consent.StatusRetired || !got.PublishedAt.Equal(*text.PublishedAt) {
		t.Fatalf("retired notice = %+v, %v", got, err)
	}
}

// TestFilterActive_Gorm_LargeBatch: FilterActive over more users than one
// IN list (consent_current view, batches of 1000).
func TestFilterActive_Gorm_LargeBatch(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialPush)
	rows, err := e.d.Pool.Query(ctx, `INSERT INTO users (status, email, email_normalized, first_name, last_name, signup_persona)
		SELECT 'active', 'bulk' || g || '@example.test', 'bulk' || g || '@example.test', 'Ada', 'Yılmaz', 'company'
		FROM generate_series(1, 1203) g RETURNING id`)
	if err != nil {
		t.Fatal(err)
	}
	var users []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		users = append(users, id)
	}
	rows.Close()
	// Every other user granted, then the first of each 10 withdrew again.
	_, err = e.d.Pool.Exec(ctx, `INSERT INTO consents (user_id, purpose_code, action, notice_version_id, text_sha256, channel, occurred_at)
		SELECT u, 'commercial_push', 'granted', $2, $3, 'web', now() - interval '1 hour' FROM unnest($1::uuid[]) WITH ORDINALITY AS t(u, i) WHERE i % 2 = 0`,
		db.UUIDArray(users), text.ID, text.BodySHA256)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.d.Pool.Exec(ctx, `INSERT INTO consents (user_id, purpose_code, action, channel)
		SELECT u, 'commercial_push', 'withdrawn', 'web' FROM unnest($1::uuid[]) WITH ORDINALITY AS t(u, i) WHERE i % 10 = 0`, db.UUIDArray(users))
	if err != nil {
		t.Fatal(err)
	}
	var want []uuid.UUID
	for i, u := range users {
		if n := i + 1; n%2 == 0 && n%10 != 0 {
			want = append(want, u)
		}
	}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	got, err := e.svc.FilterActive(ctx, users, consent.PurposeCommercialPush)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("FilterActive = %d users, want %d", len(got), len(want))
	}
}

// TestIYS_Gorm_DeliverAndReconcile runs the İYS delivery over PostgreSQL.
func TestIYS_Gorm_DeliverAndReconcile(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	user := insertUser(t, e.d, uuid.NewV7())
	text := e.consentText(t, consent.PurposeCommercialEmail)
	id, err := e.svc.Record(ctx, grant(user, text, consent.ChannelWeb))
	if err != nil {
		t.Fatal(err)
	}
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	ev, _ := domainevent.New(domainevent.ConsentRecorded{ConsentID: id, UserID: user, Purpose: "commercial_email", Action: "granted"}, "consent", id)

	e.iys.Fail(errors.New("iys unavailable"))
	if err := r.For(consent.TopicRecorded)[0].Handle(ctx, ev); err == nil {
		t.Fatal("delivery while İYS is down succeeded")
	}
	sub, _ := e.store.SubmissionOf(ctx, id)
	if sub.Status != consent.SubmissionFailed || sub.Attempts != 1 {
		t.Fatalf("after failure = %+v", sub)
	}
	e.iys.Fail(nil)
	e.clk.Set(time.Now().Add(consent.ReconcileGrace + time.Minute)) // updated_at is the database clock
	for _, j := range e.mod.Jobs() {
		if err := j.Handle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sub, _ = e.store.SubmissionOf(ctx, id)
	if sub.Status != consent.SubmissionSent || sub.Attempts != 2 || sub.SentAt == nil || sub.LastError != nil {
		t.Fatalf("after reconcile = %+v", sub)
	}
}

// TestUnsubscribe_Gorm_ConcurrentClicksWithdrawOnce (BE-062): a mailbox
// provider retrying the one-click POST, or several clicks at once, withdraw
// the consent once — one withdrawn row, one İYS submission, one outbox
// event, one audit row — because the (user, purpose) advisory lock makes
// the "latest row is a grant" check and the append atomic.
func TestUnsubscribe_Gorm_ConcurrentClicksWithdrawOnce(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t, withLinks(unsubBase, deriver(t, tokenKey("k1", 0x11))))
	uid := insertUser(t, e.d, uuid.NewV7())
	text := e.consentText(t, consent.PurposeCommercialEmail)
	if _, err := e.svc.Record(t.Context(), grant(uid, text, consent.ChannelWeb)); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Second)
	link, err := e.mod.Links.UnsubscribeURL(t.Context(), uid, consent.PurposeCommercialEmail)
	if err != nil {
		t.Fatal(err)
	}
	tok := link[len(unsubBase)+1:]

	const clicks = 8
	errs := make(chan error, clicks)
	start := make(chan struct{})
	for range clicks {
		go func() {
			<-start
			errs <- e.svc.Unsubscribe(context.Background(), tok)
		}()
	}
	close(start)
	for range clicks {
		if err := <-errs; err != nil {
			t.Fatalf("Unsubscribe: %v", err)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM consents WHERE user_id = $1 AND action = 'withdrawn' AND channel = 'email_link'`, uid); n != 1 {
		t.Fatalf("withdrawn rows = %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM iys_submissions s JOIN consents c ON c.id = s.consent_id
		WHERE c.user_id = $1 AND c.action = 'withdrawn'`, uid); n != 1 {
		t.Fatalf("İYS submissions of the withdrawal = %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM outbox WHERE topic = $1`, domainevent.TopicConsentRecorded); n != 2 {
		t.Fatalf("consent.recorded events = %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'consent.record.create' AND actor_user_id = $1
		AND metadata->>'channel' = 'email_link'`, uid); n != 1 {
		t.Fatalf("audit rows of the withdrawal = %d, want 1", n)
	}
	if ok, err := e.svc.HasActive(t.Context(), uid, consent.PurposeCommercialEmail, nil); err != nil || ok {
		t.Fatalf("HasActive after unsubscribe = %v, %v", ok, err)
	}
}

// Failing the last write must leave the grant intact and roll back every
// withdrawal artifact; retrying the same link can then complete normally.
func TestUnsubscribe_Gorm_OutboxFailureRollsBackWithdrawal(t *testing.T) {
	t.Parallel()
	var failing *unsubscribeFailOutbox
	e := newPGEnv(t, withLinks(unsubBase, deriver(t, tokenKey("k1", 0x11))), func(d *consent.Deps) {
		failing = &unsubscribeFailOutbox{Writer: d.Outbox}
		d.Outbox = failing
	})
	ctx, purpose := t.Context(), consent.PurposeCommercialEmail
	uid := insertUser(t, e.d, uuid.NewV7())
	if _, err := e.svc.Record(ctx, grant(uid, e.consentText(t, purpose), consent.ChannelWeb)); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Second)
	link, err := e.mod.Links.UnsubscribeURL(t.Context(), uid, purpose)
	if err != nil {
		t.Fatal(err)
	}
	tok := link[len(unsubBase)+1:]
	counts := func() [4]int {
		return [4]int{
			e.count(t, `SELECT count(*) FROM consents WHERE user_id = $1`, uid),
			e.count(t, `SELECT count(*) FROM iys_submissions`),
			e.count(t, `SELECT count(*) FROM outbox WHERE topic = $1`, domainevent.TopicConsentRecorded),
			e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'consent.record.create' AND actor_user_id = $1`, uid),
		}
	}
	before := counts()
	failing.fail.Store(true)
	if err := e.svc.Unsubscribe(ctx, tok); !errors.Is(err, errBoom) {
		t.Fatalf("Unsubscribe with failing outbox = %v, want injected failure", err)
	}
	if after := counts(); after != before {
		t.Fatalf("failed unsubscribe persisted artifacts: before %v, after %v", before, after)
	}
	if active, err := e.svc.HasActive(ctx, uid, purpose, nil); err != nil || !active {
		t.Fatalf("HasActive after rollback = %v, %v", active, err)
	}
	failing.fail.Store(false)
	if err := e.svc.Unsubscribe(ctx, tok); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	want := before
	for i := range want {
		want[i]++
	}
	if after := counts(); after != want {
		t.Fatalf("retry artifacts = %v, want %v", after, want)
	}
	if active, err := e.svc.HasActive(ctx, uid, purpose, nil); err != nil || active {
		t.Fatalf("HasActive after retry = %v, %v", active, err)
	}
}

type unsubscribeFailOutbox struct {
	outbox.Writer
	fail atomic.Bool
}

func (w *unsubscribeFailOutbox) Add(ctx context.Context, ev domainevent.Envelope) error {
	if err := w.Writer.Add(ctx, ev); err != nil {
		return err
	}
	if w.fail.Load() {
		return errBoom
	}
	return nil
}
