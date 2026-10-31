//go:build integration

package messaging_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz/authztest"
	"koro/internal/entitlements"
	"koro/internal/messaging"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
	"koro/internal/students"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var seq atomic.Int64

func pgExec(t *testing.T, d *db.DB, sql string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func pgCompany(t *testing.T, d *db.DB, id uuid.UUID) {
	t.Helper()
	n := strconv.FormatInt(seq.Add(1), 10)
	pgExec(t, d, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'company', $2, $3)`, id, "Şirket "+n, "sirket-"+n)
}

func pgUser(t *testing.T, d *db.DB, id uuid.UUID) {
	t.Helper()
	pgExec(t, d, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, "u"+strconv.FormatInt(seq.Add(1), 10)+"@example.test")
}

// pgEntitlement gives org an active contact_request right of n units.
func pgEntitlement(t *testing.T, d *db.DB, org uuid.UUID, n int) uuid.UUID {
	t.Helper()
	admin, id := uuid.NewV7(), uuid.NewV7()
	pgUser(t, d, admin)
	pgExec(t, d, `INSERT INTO entitlements (id, org_id, feature, kind, quantity_total, valid_from, valid_to, status,
		source_type, granted_by, activated_at) VALUES ($1, $2, 'contact_request', 'quota', $3, now() - interval '1 day',
		now() + interval '30 days', 'active', 'admin_grant', $4, now())`, id, org, n, admin)
	return id
}

// pgWorld inserts the parent rows (organizations, users, a usage) as app_rw.
func pgWorld(d *db.DB) world {
	return world{
		org: func(t *testing.T) uuid.UUID {
			id := uuid.NewV7()
			pgCompany(t, d, id)
			return id
		},
		user: func(t *testing.T) uuid.UUID {
			id := uuid.NewV7()
			pgUser(t, d, id)
			return id
		},
		usage: func(t *testing.T, org uuid.UUID) uuid.UUID {
			ent, id := pgEntitlement(t, d, org, 1), uuid.NewV7()
			pgExec(t, d, `INSERT INTO entitlement_usages (id, entitlement_id, org_id, quantity, ref_type, ref_id)
				VALUES ($1, $2, $3, 1, 'contact_request', $4)`, id, ent, org, uuid.NewV7())
			return id
		},
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (messaging.Store, world) {
		d := testutil.NewDB(t)
		return messaging.NewGormStore(d.ORM), pgWorld(d)
	})
}

func TestConversationStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	conversationContract(t, func(t *testing.T) (messaging.Store, world) {
		d := testutil.NewDB(t)
		return messaging.NewGormStore(d.ORM), pgWorld(d)
	})
}

// pgEntitlements consumes a unit as entitlements does (a usage row and the
// counter in the caller's transaction), so the request's FK holds and a
// rollback takes the unit back.
type pgEntitlements struct {
	d   *db.DB
	ent uuid.UUID
}

func (p pgEntitlements) Consume(ctx context.Context, org uuid.UUID, _ entitlements.Feature, _ int, ref entitlements.Ref) (uuid.UUID, error) {
	res := db.Conn(ctx, p.d.ORM).Exec(`UPDATE entitlements SET quantity_used = quantity_used + 1
		WHERE id = @id AND quantity_used < quantity_total`, map[string]any{"id": p.ent})
	if res.Error != nil {
		return uuid.Nil(), res.Error
	}
	if res.RowsAffected == 0 {
		return uuid.Nil(), entitlements.ErrNoEntitlement
	}
	id := uuid.NewV7()
	err := db.Conn(ctx, p.d.ORM).Exec(`INSERT INTO entitlement_usages (id, entitlement_id, org_id, quantity, ref_type, ref_id)
		VALUES ( @id, @ent, @org, 1, 'contact_request', @ref)`, map[string]any{"id": id, "ent": p.ent, "org": org, "ref": ref.ID}).Error
	return id, err
}

// pgGrants writes the level 3 grant in the caller's transaction (students'
// row), failing on demand after the write.
type pgGrants struct {
	d    *db.DB
	fail *atomic.Bool
}

func (p pgGrants) Grant(ctx context.Context, student, company uuid.UUID, src students.GrantSource, srcID uuid.UUID) error {
	err := db.Conn(ctx, p.d.ORM).Exec(`INSERT INTO profile_access_grants (id, student_user_id, company_org_id, source, source_id)
		VALUES ( @id, @student, @company, @src, @src_id)`, map[string]any{
		"id": uuid.NewV7(), "student": student, "company": company, "src": string(src), "src_id": srcID,
	}).Error
	if err != nil {
		return err
	}
	if p.fail.Load() {
		return students.ErrMinorRestricted
	}
	return nil
}

type pgAudit struct{}

func (pgAudit) Log(ctx context.Context, _ audit.Event) error {
	if !db.InTx(ctx) {
		return errors.New("audit outside a transaction")
	}
	return nil
}

// TestService_Postgres_CreateAcceptAndExpire runs messaging over
// PostgreSQL (READ COMMITTED transactions of the app_rw pool, the real
// outbox and idempotency stores): the unit, the row and
// contact_request.created commit together and a replay of the
// Idempotency-Key consumes nothing; a failing level 3 grant rolls the
// acceptance back (the request stays pending, no grant, no closed event);
// the acceptance commits with its grant; the expiry job closes a due
// request; the sweeper deletes closed requests 12 months on.
func TestService_Postgres_CreateAcceptAndExpire(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	companyA := authztest.ID("company_a")
	cmm, stu, other := actor(t, "company_member"), actor(t, "student"), actor(t, "student_granted")
	pgCompany(t, d, companyA)
	for _, id := range []uuid.UUID{cmm.UserID, stu.UserID, other.UserID} {
		pgUser(t, d, id)
	}
	ent := pgEntitlement(t, d, companyA, 5)
	fail := &atomic.Bool{}
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: outbox.NewRegistry(),
		Queue: queue.NewMem(), Clock: clk, Log: obs.Discard()})
	keeper := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()})
	sf := &studentsFake{discoverable: map[uuid.UUID]bool{stu.UserID: true, other.UserID: true}}
	res := &resolver{students: sf, known: []uuid.UUID{stu.UserID, other.UserID}}
	of := &orgsFake{orgs: map[uuid.UUID]orgs.OrgSummary{
		companyA: {ID: companyA, Kind: orgs.KindCompany, Name: "Şirket A", Status: orgs.StatusActive},
	}}
	store := messaging.NewGormStore(d.ORM)
	mod, err := messaging.New(messaging.Deps{
		Store: store, Tx: d.Tx, Outbox: ob, Audit: pgAudit{}, Idempotency: keeper, Clock: clk, Log: obs.Discard(),
		Entitlements: pgEntitlements{d: d, ent: ent}, Grants: pgGrants{d: d, fail: fail},
		Contacts: pgShares{d: d, fail: &atomic.Bool{}}, Students: sf, Orgs: of,
		Pseudonyms: pseudonymsFake{}, Resolve: res.resolve,
	})
	if err != nil {
		t.Fatal(err)
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}

	kctx := withKey(ctx, cmm, "pg-create-0001")
	in := request(companyA, "student")
	v, err := mod.Service.Create(kctx, cmm, companyA, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mod.Service.Create(kctx, cmm, companyA, in); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := count(`SELECT quantity_used FROM entitlements WHERE id = $1`, ent); n != 1 {
		t.Fatalf("units used = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM outbox WHERE topic = 'contact_request.created'`); n != 1 {
		t.Fatalf("created events = %d", n)
	}
	if n := count(`SELECT count(*) FROM outbox WHERE topic = 'contact_request.created' AND payload::text LIKE '%' || $1 || '%'`,
		in.Message); n != 0 {
		t.Fatal("the message is in the event")
	}

	fail.Store(true)
	_, err = mod.Service.Respond(ctx, stu, v.Request.ID, accept)
	problemtest.WantErr(t, err, students.ErrMinorRestricted)
	if st := count(`SELECT count(*) FROM contact_requests WHERE id = $1 AND status = 'pending' AND responded_at IS NULL`, v.Request.ID); st != 1 ||
		count(`SELECT count(*) FROM profile_access_grants WHERE source_id = $1`, v.Request.ID) != 0 ||
		count(`SELECT count(*) FROM outbox WHERE topic = 'contact_request.closed'`) != 0 ||
		count(`SELECT count(*) FROM conversations WHERE contact_request_id = $1`, v.Request.ID) != 0 {
		t.Fatal("a failed grant did not roll the acceptance back")
	}
	fail.Store(false)
	if _, err := mod.Service.Respond(ctx, stu, v.Request.ID, accept); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM contact_requests WHERE id = $1 AND status = 'accepted' AND responded_at IS NOT NULL`, v.Request.ID) != 1 ||
		count(`SELECT count(*) FROM profile_access_grants WHERE source_id = $1 AND student_user_id = $2`, v.Request.ID, stu.UserID) != 1 ||
		count(`SELECT count(*) FROM outbox WHERE topic = 'contact_request.closed'`) != 1 {
		t.Fatal("the acceptance did not commit with its grant and event")
	}
	// BE-162: the conversation of the acceptance, the company's from the
	// request; a message, its event (IDs only) and the activity commit
	// together.
	var conv uuid.UUID
	if err := d.Pool.QueryRow(ctx, `SELECT id FROM conversations WHERE contact_request_id = $1 AND org_id = $2
		AND student_user_id = $3 AND company_user_id = $4 AND status = 'open'`, v.Request.ID, companyA, stu.UserID, cmm.UserID).
		Scan(&conv); err != nil {
		t.Fatalf("the acceptance opened no conversation: %v", err)
	}
	if _, err := mod.Service.SendMessage(ctx, cmm, messaging.CompanyParty(companyA), conv, "PostgreSQL üzerinden merhaba"); err != nil {
		t.Fatal(err)
	}
	reply, err := mod.Service.SendMessage(ctx, stu, messaging.StudentParty(), conv, "Merhaba, teşekkürler")
	if err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM outbox WHERE topic = 'conversation.message_sent'`) != 2 ||
		count(`SELECT count(*) FROM outbox WHERE topic = 'conversation.message_sent' AND payload::text LIKE '%merhaba%'`) != 0 ||
		count(`SELECT company_unanswered FROM conversations WHERE id = $1`, conv) != 0 ||
		count(`SELECT count(*) FROM conversation_user_states WHERE conversation_id = $1 AND last_read_message_id = $2`, conv, reply.ID) != 1 {
		t.Fatal("the messages did not commit with their events, counter and the sender's read marker")
	}
	cv, err := mod.Service.GetConversation(ctx, cmm, messaging.CompanyParty(companyA), conv)
	if err != nil || cv.Unread != 1 || cv.Last == nil || cv.Last.ID != reply.ID || !cv.CanSend {
		t.Fatalf("company view = %+v, %v", cv, err)
	}

	due, err := mod.Service.Create(ctx, cmm, companyA, request(companyA, "student_granted"))
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(messaging.RequestTTL + time.Minute)
	if err := mod.ExpireDue(ctx); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM contact_requests WHERE id = $1 AND status = 'expired'`, due.Request.ID) != 1 ||
		count(`SELECT quantity_used FROM entitlements WHERE id = $1`, ent) != 2 {
		t.Fatal("expiry did not close the request, or gave the unit back")
	}

	p := privacy.RetentionPolicy{Code: messaging.PolicyContactRequestClosed, Retention: 365 * 24 * time.Hour, LegalApproved: true}
	r, err := mod.Sweeper.Sweep(ctx, p, time.Now().Add(364*24*time.Hour))
	if err != nil || r.Count != 0 {
		t.Fatalf("sweep before 12 months = %+v, %v", r, err)
	}
	r, err = mod.Sweeper.Sweep(ctx, p, time.Now().Add(366*24*time.Hour))
	if err != nil || r.Count != 2 || count(`SELECT count(*) FROM contact_requests`) != 0 {
		t.Fatalf("sweep after 12 months = %+v, %v", r, err)
	}
	if count(`SELECT count(*) FROM conversations WHERE id = $1 AND contact_request_id IS NULL`, conv) != 1 {
		t.Fatal("the conversation did not outlive its request (ON DELETE SET NULL)")
	}
	p = privacy.RetentionPolicy{Code: messaging.PolicyConversationClosed, Retention: 365 * 24 * time.Hour, LegalApproved: true}
	if r, err = mod.Sweeper.Sweep(ctx, p, time.Now().Add(366*24*time.Hour)); err != nil || r.Count != 1 ||
		count(`SELECT count(*) FROM conversation_messages`) != 0 || count(`SELECT count(*) FROM conversation_user_states`) != 0 {
		t.Fatalf("conversation sweep = %+v, %v", r, err)
	}
}

func TestStatsStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	statsContract(t, func(t *testing.T) (messaging.Store, world) {
		d := testutil.NewDB(t)
		return messaging.NewGormStore(d.ORM), pgWorld(d)
	})
}
