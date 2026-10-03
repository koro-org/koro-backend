package audit_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz/authztest"
)

// The chain job links rows in seq order: chain_seq 1, 2, …; the first
// prev_hash is 32 zero bytes, every later one is the previous row_hash, and
// row_hash = SHA-256(prev_hash ‖ canonical row) (02 §2.1).
func TestChain_SequentialHashes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := t.Context()
	org := authztest.ID("club_a")
	e.logN(t, ctx, 5, "events.event.create", authztest.ID("club_admin"), &org)
	if n := e.chain(t); n != 5 {
		t.Fatalf("Chain() linked %d rows, want 5", n)
	}
	if n := e.chain(t); n != 0 {
		t.Fatalf("second Chain() linked %d rows, want 0 (idempotent)", n)
	}
	e.logN(t, ctx, 2, "events.event.update", authztest.ID("club_admin"), &org)
	if n := e.chain(t); n != 2 {
		t.Fatalf("Chain() after two more events linked %d rows", n)
	}

	rows := e.chained(t)
	if len(rows) != 7 {
		t.Fatalf("%d chained rows, want 7", len(rows))
	}
	prev := make([]byte, audit.HashSize)
	for i, r := range rows {
		if *r.ChainSeq != int64(i+1) || r.Seq != int64(i+1) {
			t.Fatalf("row %d: chain_seq %d, seq %d", i, *r.ChainSeq, r.Seq)
		}
		if !bytes.Equal(r.PrevHash, prev) {
			t.Fatalf("row %d: prev_hash is not the previous row_hash", i)
		}
		want, err := audit.RowHash(prev, r)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(r.RowHash, want) || len(r.RowHash) != audit.HashSize {
			t.Fatalf("row %d: row_hash does not match SHA-256(prev ‖ canonical row)", i)
		}
		if r.ChainedAt == nil {
			t.Fatalf("row %d: chained_at not set", i)
		}
		prev = r.RowHash
	}
	if res, err := audit.VerifyRange(ctx, e.m, day0, day0); err != nil || !res.OK || res.CheckedRows != 7 {
		t.Fatalf("VerifyRange() = %+v, %v", res, err)
	}
}

// tamperEnv is a chain of 10 rows on day0 with a signed root of day0, and
// 5 rows of the next day without a root yet.
func tamperEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, nil)
	ctx := t.Context()
	e.logN(t, ctx, 10, "identity.session.create", authztest.ID("student"), nil)
	e.chain(t)
	e.clk.Set(t0.Add(24 * time.Hour)) // next day, after 00:15
	if err := audit.DailyRoots(ctx, e.m); err != nil {
		t.Fatal(err)
	}
	e.logN(t, ctx, 5, "identity.session.create", authztest.ID("student"), nil)
	e.chain(t)
	return e
}

// Tampering with the log breaks verification (01 §4.1, ticket BE-007):
// an edited, deleted or inserted row is found at its chain position, and a
// rewritten (re-hashed) chain no longer matches the signed daily root.
func TestVerifyChain_DetectsTamper(t *testing.T) {
	t.Parallel()
	forged := func() audit.Record {
		u := authztest.ID("platform_admin")
		return audit.Record{ID: uuid.NewV7(), OccurredAt: t0, ActorKind: "user", ActorUserID: &u,
			Action: "kvkk.dsr.approve", Outcome: audit.Success, Metadata: json.RawMessage(`{}`)}
	}
	tests := []struct {
		name    string
		tamper  func(s *audit.MemStore)
		wantSeq int64
		problem string
	}{
		{"intact", func(*audit.MemStore) {}, 0, ""},
		{"edited column", func(s *audit.MemStore) {
			s.TamperEdit(4, func(r *audit.Record) { r.Outcome = audit.Denied })
		}, 4, "row_hash"},
		{"edited metadata", func(s *audit.MemStore) {
			s.TamperEdit(7, func(r *audit.Record) { r.Metadata = json.RawMessage(`{"n": 99}`) })
		}, 7, "row_hash"},
		{"deleted row", func(s *audit.MemStore) { s.TamperDelete(5) }, 5, "gap"},
		{"deleted row, chain renumbered and re-hashed", func(s *audit.MemStore) {
			s.TamperDelete(5)
			s.TamperRenumber(5)
		}, 10, "root"}, // the root of day0 points at seq 10, which now is a later row
		{"deleted last row of the signed day", func(s *audit.MemStore) {
			s.TamperDelete(10)
			s.TamperRenumber(10)
		}, 10, "root"},
		{"inserted row", func(s *audit.MemStore) { s.TamperInsertAt(3, forged(), false) }, 4, "prev_hash"},
		{"inserted row, chain re-hashed", func(s *audit.MemStore) { s.TamperInsertAt(3, forged(), true) }, 10, "root"},
		{"root hash replaced", func(s *audit.MemStore) {
			s.TamperEdit(10, func(r *audit.Record) { r.Reason = "covered up" })
			s.TamperRenumber(10) // re-hash from 10 on: rows agree, the signed root does not
		}, 10, "root"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := tamperEnv(t)
			tc.tamper(e.store)
			res, err := audit.VerifyRange(t.Context(), e.m, day0, day0.AddDays(1))
			if err != nil {
				t.Fatal(err)
			}
			if tc.problem == "" {
				if !res.OK || res.CheckedRows != 15 {
					t.Fatalf("intact chain: %+v", res)
				}
				return
			}
			if res.OK || res.FirstBadChainSeq == nil || *res.FirstBadChainSeq != tc.wantSeq || res.Problem != tc.problem {
				got := int64(-1)
				if res.FirstBadChainSeq != nil {
					got = *res.FirstBadChainSeq
				}
				t.Fatalf("Verify = ok %v, first bad %d (%s); want %d (%s)", res.OK, got, res.Problem, tc.wantSeq, tc.problem)
			}
		})
	}
}

// A forged signature (or a root signed by an unknown key) is found even
// when the rows agree with it.
func TestVerifyChain_DetectsForgedRoot(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := t.Context()
	e.logN(t, ctx, 3, "identity.session.create", authztest.ID("student"), nil)
	e.chain(t)
	other, err := audit.NewEd25519Signer(testKeys[:0:0])
	if err == nil || other != nil {
		t.Fatal("a signer without keys must fail")
	}
	rows := e.chained(t)
	bad := audit.DailyRoot{Day: day0, LastChainSeq: 3, RootHash: rows[2].RowHash, Signature: make([]byte, 64), KeyID: "test1", ExternalRef: "x"}
	if err := e.store.InsertRoot(ctx, bad); err != nil {
		t.Fatal(err)
	}
	res, err := audit.VerifyRange(ctx, e.m, day0, day0)
	if err != nil || res.OK || res.Problem != "signature" || *res.FirstBadChainSeq != 3 {
		t.Fatalf("VerifyRange() = %+v, %v; want a signature finding at 3", res, err)
	}
}

// audit.daily_root signs the chain head at the end of every finished day,
// archives a write-once copy and is idempotent.
func TestDailyRoot_SignedArchivedIdempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := t.Context()
	e.logN(t, ctx, 4, "identity.session.create", authztest.ID("student"), nil)
	e.chain(t)
	// day0 (4 rows), then the quiet day0+1; each run twice (idempotent).
	for _, at := range []time.Time{t0.Add(24 * time.Hour), t0.Add(48 * time.Hour)} {
		e.clk.Set(at)
		for range 2 {
			if err := audit.DailyRoots(ctx, e.m); err != nil {
				t.Fatal(err)
			}
		}
	}
	roots, err := e.store.Roots(ctx, day0.AddDays(-5), day0.AddDays(5))
	if err != nil || len(roots) != 2 {
		t.Fatalf("roots = %+v, %v; want day0 and the quiet next day", roots, err)
	}
	rows := e.chained(t)
	for _, r := range roots {
		if r.LastChainSeq != 4 || !bytes.Equal(r.RootHash, rows[3].RowHash) || r.KeyID != "test1" || len(r.Signature) != 64 {
			t.Fatalf("root %s = %+v", r.Day, r)
		}
		doc, ok := e.archive.Get(r.ExternalRef)
		if !ok || r.ExternalRef != "audit-roots/"+r.Day.String()+".json" {
			t.Fatalf("root %s: archive copy %q missing", r.Day, r.ExternalRef)
		}
		var d struct {
			Day          string `json:"day"`
			LastChainSeq int64  `json:"last_chain_seq"`
			KeyID        string `json:"key_id"`
		}
		if err := json.Unmarshal(doc, &d); err != nil || d.Day != r.Day.String() || d.LastChainSeq != 4 || d.KeyID != "test1" {
			t.Fatalf("archived root = %s (%v)", doc, err)
		}
	}
	if res, err := audit.VerifyRange(ctx, e.m, day0, day0.AddDays(1)); err != nil || !res.OK {
		t.Fatalf("VerifyRange() = %+v, %v", res, err)
	}
	if len(e.Alarms()) != 0 {
		t.Fatalf("alarms = %+v", e.Alarms())
	}
}

// Without a signing key the job fails loudly (no made-up key, 05 §12.2),
// and raises the alarm.
func TestDailyRoot_WithoutSignerFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *audit.Deps) { d.Signer = nil })
	err := audit.DailyRoots(t.Context(), e.m)
	if !errors.Is(err, audit.ErrSignerNotConfigured) {
		t.Fatalf("DailyRoots() error = %v", err)
	}
	if a := e.Alarms(); len(a) != 1 || a[0].Kind != audit.AlarmRootFailed {
		t.Fatalf("alarms = %+v", a)
	}
	e2 := newEnv(t, func(d *audit.Deps) { d.Archive = nil })
	if err := audit.DailyRoots(t.Context(), e2.m); !errors.Is(err, audit.ErrArchiveNotConfigured) {
		t.Fatalf("DailyRoots() without archive error = %v", err)
	}
}

// A different object under a root's key means the write-once copy was
// changed: the job refuses to continue.
func TestDailyRoot_ArchiveConflict(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	if _, err := e.archive.Put(t.Context(), "audit-roots/"+day0.String()+".json", []byte(`{"forged":true}`)); err != nil {
		t.Fatal(err)
	}
	e.clk.Set(t0.Add(24 * time.Hour))
	if err := audit.DailyRoots(t.Context(), e.m); !errors.Is(err, audit.ErrArchiveConflict) {
		t.Fatalf("DailyRoots() error = %v, want ErrArchiveConflict", err)
	}
}

// audit.verify_chain raises the alarm on tampering, a missing root and a
// lagging chain, and records the check.
func TestVerifyRecent_Alarms(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	e := tamperEnv(t)
	if err := audit.VerifyRecent(ctx, e.m); err != nil {
		t.Fatal(err)
	}
	if a := e.Alarms(); len(a) != 0 {
		t.Fatalf("intact chain raised %+v", a)
	}
	e.store.TamperDelete(2)
	if err := audit.VerifyRecent(ctx, e.m); err != nil {
		t.Fatal(err)
	}
	if a := e.Alarms(); len(a) != 1 || a[0].Kind != audit.AlarmChainBroken || *a[0].FirstBadChainSeq != 2 {
		t.Fatalf("alarms = %+v, want chain_broken at 2", a)
	}

	lag := newEnv(t, nil)
	lag.logN(t, ctx, 1, "identity.session.create", authztest.ID("student"), nil)
	lag.clk.Advance(time.Hour) // never chained, no root for yesterday
	if err := audit.VerifyRecent(ctx, lag.m); err != nil {
		t.Fatal(err)
	}
	kinds := map[audit.AlarmKind]bool{}
	for _, a := range lag.Alarms() {
		kinds[a.Kind] = true
	}
	if !kinds[audit.AlarmChainLag] || !kinds[audit.AlarmRootFailed] {
		t.Fatalf("alarms = %+v, want chain_lag and root_failed", lag.Alarms())
	}
	lag.chain(t)
	last := lag.chained(t)
	if r := last[len(last)-1]; r.Action != "audit.chain.verify" || r.ActorKind != "system" {
		t.Fatalf("the verification was not recorded: %+v", r)
	}
}
