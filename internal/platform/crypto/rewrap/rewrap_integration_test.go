//go:build integration

package rewrap_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/crypto/openbao"
	"koro/internal/platform/crypto/openbao/openbaotest"
	"koro/internal/platform/crypto/rewrap"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

const purposePrefix = "test.secret/"

var column = rewrap.Column{
	Name: "test.rewrap_secrets.secret_enc", Table: "rewrap_secrets", Key: "id", Column: "secret_enc",
	PurposePrefix: purposePrefix, PurposeColumn: "owner_id",
}

type fixture struct {
	d     *db.DB
	local *crypto.Envelope // the pilot KEK alone (values sealed before the switch)
	env   *crypto.Envelope // OpenBao active, local KEK for old values
	clk   *clock.Fake
	rows  map[uuid.UUID]string // id → plaintext
	owner map[uuid.UUID]uuid.UUID
}

// newFixture creates a table with n values sealed under the local KEK, a
// NULL value and a corrupt one.
func newFixture(t *testing.T, transit *openbao.Transit, clk *clock.Fake, n int) *fixture {
	t.Helper()
	d := testutil.NewDB(t)
	owner := testutil.OwnerConn(t, d)
	if _, err := owner.Exec(t.Context(), `CREATE TABLE rewrap_secrets (id uuid PRIMARY KEY, owner_id uuid NOT NULL, secret_enc bytea);
		GRANT SELECT, INSERT, UPDATE ON rewrap_secrets TO app_rw`); err != nil {
		t.Fatal(err)
	}
	kek, err := crypto.NewLocalKEK([]crypto.Key{{ID: "pilot1", Secret: bytes.Repeat([]byte{9}, crypto.KeySize)}})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := crypto.NewEnvelope(kek)
	chain, err := crypto.NewChain(transit, kek)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := crypto.NewEnvelope(chain)
	f := &fixture{d: d, local: local, env: env, clk: clk, rows: map[uuid.UUID]string{}, owner: map[uuid.UUID]uuid.UUID{}}
	for i := range n {
		id, ownerID := uuid.NewV7(), uuid.NewV7()
		pt := fmt.Sprintf("secret-%03d", i)
		sealed, err := local.Seal(t.Context(), purposePrefix+ownerID.String(), []byte(pt))
		if err != nil {
			t.Fatal(err)
		}
		f.insert(t, id, ownerID, sealed)
		f.rows[id], f.owner[id] = pt, ownerID
	}
	f.insert(t, uuid.NewV7(), uuid.NewV7(), nil)
	return f
}

func (f *fixture) insert(t *testing.T, id, ownerID uuid.UUID, sealed []byte) {
	t.Helper()
	if err := f.d.ORM.WithContext(t.Context()).Exec(`INSERT INTO rewrap_secrets (id, owner_id, secret_enc) VALUES ( ?, ?, ? )`,
		id, ownerID, sealed).Error; err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) value(t *testing.T, id uuid.UUID) []byte {
	t.Helper()
	var out []byte
	if err := f.d.ORM.WithContext(t.Context()).Raw(`SELECT secret_enc FROM rewrap_secrets WHERE id = ?`, id).Row().Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// keys counts the stored values per key id.
func (f *fixture) keys(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for id := range f.rows {
		kid, err := crypto.KeyID(f.value(t, id))
		if err != nil {
			t.Fatal(err)
		}
		out[kid]++
	}
	return out
}

func (f *fixture) job(t *testing.T, env rewrap.Envelope, log *slog.Logger) *rewrap.Job {
	t.Helper()
	j, err := rewrap.New(rewrap.Deps{
		ORM: f.d.ORM, Tx: f.d.Tx, Envelope: env, Columns: []rewrap.Column{column}, Clock: f.clk, Log: log,
		BatchSize: 10, Budget: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// slowEnvelope advances the clock by one second per value (so a run hits
// its 10 s budget after one batch of 10) and can change a row behind the
// job's back.
type slowEnvelope struct {
	*crypto.Envelope
	clk    *clock.Fake
	mu     sync.Mutex
	before func(ctx context.Context, purpose string)
}

func (s *slowEnvelope) Rewrap(ctx context.Context, purpose string, sealed []byte) ([]byte, bool, error) {
	s.clk.Advance(time.Second)
	s.mu.Lock()
	hook := s.before
	s.mu.Unlock()
	if hook != nil {
		hook(ctx, purpose)
	}
	return s.Envelope.Rewrap(ctx, purpose, sealed)
}

// TestRewrap_LocalToOpenBao_Batches moves values sealed under the local
// pilot KEK to OpenBao Transit (real dev container): in batches, resumable
// after the time budget, idempotent, safe against a concurrent write (the
// compare-and-swap leaves the newer value) and without touching NULL or
// corrupt values.
func TestRewrap_LocalToOpenBao_Batches(t *testing.T) {
	t.Parallel()
	srv := openbaotest.Start(t, "koro-kek")
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	transit, err := openbao.New(openbao.Config{Addr: srv.Addr, Token: srv.AppToken, Key: srv.Key, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, transit, clk, 25)
	corruptID := uuid.NewV7()
	f.insert(t, corruptID, uuid.NewV7(), []byte{0x01, 0x06, 'p', 'i', 'l', 'o', 't', '1', 0, 3, 1, 2, 3})
	var logs strings.Builder
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	slow := &slowEnvelope{Envelope: f.env, clk: clk}
	job := f.job(t, slow, log)
	ctx := t.Context()

	// Run 1 stops at the budget after one batch of 10.
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res.Rewrapped != 10 || res.Complete {
		t.Fatalf("run 1 = %+v, want 10 rewrapped and incomplete", res)
	}
	if got := f.keys(t); got["bao.koro-kek.v1"] != 10 || got["pilot1"] != 15 {
		t.Fatalf("after run 1 keys = %v", got)
	}

	// Run 2: the application re-enrolls one secret while the job works on it.
	// The row raced is the first one run 2 takes (keyset order on id).
	var raced uuid.UUID
	for id := range f.rows {
		if kid, _ := crypto.KeyID(f.value(t, id)); kid == "pilot1" && (raced == uuid.Nil() || id.Compare(raced) < 0) {
			raced = id
		}
	}
	newer, _ := f.local.Seal(ctx, purposePrefix+f.owner[raced].String(), []byte("re-enrolled"))
	slow.before = func(ctx context.Context, purpose string) {
		if purpose == purposePrefix+f.owner[raced].String() {
			if err := f.d.ORM.WithContext(ctx).Exec(`UPDATE rewrap_secrets SET secret_enc = ? WHERE id = ?`, newer, raced).Error; err != nil {
				t.Error(err)
			}
		}
	}
	res, err = job.Run(ctx)
	if err != nil || res.Rewrapped != 9 || res.Conflicts != 1 || res.Complete {
		t.Fatalf("run 2 = %+v, %v; want 9 rewrapped, 1 conflict, incomplete", res, err)
	}
	if v := f.value(t, raced); !bytes.Equal(v, newer) {
		t.Fatal("the job overwrote a value the application changed meanwhile")
	}
	slow.before = nil
	f.rows[raced] = "re-enrolled"

	// Run 3 finishes (the conflicting row included); the corrupt value is
	// reported, never rewritten.
	res, err = job.Run(ctx)
	if err != nil || !res.Complete || res.Rewrapped != 6 || res.Failed != 1 {
		t.Fatalf("run 3 = %+v, %v; want complete with 6 rewrapped and 1 failed", res, err)
	}
	if got := f.keys(t); got["bao.koro-kek.v1"] != 25 || len(got) != 1 {
		t.Fatalf("after run 3 keys = %v", got)
	}
	if v := f.value(t, corruptID); !bytes.Equal(v, []byte{0x01, 0x06, 'p', 'i', 'l', 'o', 't', '1', 0, 3, 1, 2, 3}) {
		t.Fatal("corrupt value was modified")
	}
	if !strings.Contains(logs.String(), `"alarm":true`) || !strings.Contains(logs.String(), corruptID.String()) {
		t.Fatalf("no alarm for the corrupt value: %s", logs.String())
	}

	// Every value opens to its plaintext with OpenBao alone in the chain's
	// active position; a fourth run changes nothing.
	for id, pt := range f.rows {
		got, err := f.env.Open(ctx, purposePrefix+f.owner[id].String(), f.value(t, id))
		if err != nil || string(got) != pt {
			t.Fatalf("row %s opens to %q, %v; want %q", id, got, err, pt)
		}
	}
	res, err = job.Run(ctx)
	if err != nil || res.Rewrapped != 0 || res.Conflicts != 0 {
		t.Fatalf("run 4 = %+v, %v; want a no-op", res, err)
	}

	// After a Transit key rotation the same job moves everything to v2.
	srv.Rotate(t)
	clk.Advance(2 * time.Minute)
	for res.Rewrapped < 25 {
		r, err := job.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r.Rewrapped == 0 {
			t.Fatalf("rotation run made no progress: %+v", r)
		}
		res.Rewrapped += r.Rewrapped
	}
	if got := f.keys(t); got["bao.koro-kek.v2"] != 25 {
		t.Fatalf("after rotation keys = %v", got)
	}
	if strings.Contains(logs.String(), "secret-0") || strings.Contains(logs.String(), srv.AppToken) {
		t.Fatal("plaintext or token in the job log")
	}
}

// TestRewrap_OpenBaoDown_FailsClosed: with the key service down the run
// stops with an error and leaves every value as it was.
func TestRewrap_OpenBaoDown_FailsClosed(t *testing.T) {
	t.Parallel()
	fake := openbaotest.NewFake(t, "koro-kek")
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	transit, err := openbao.New(openbao.Config{Addr: fake.Server.URL, Token: openbaotest.FakeToken, Key: "koro-kek", Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, transit, clk, 5)
	if _, err := transit.ActiveKeyID(t.Context()); err != nil { // cache the active key, then fail
		t.Fatal(err)
	}
	before := map[uuid.UUID][]byte{}
	for id := range f.rows {
		before[id] = f.value(t, id)
	}
	fake.SetDown(true)
	res, err := f.job(t, f.env, slog.New(slog.DiscardHandler)).Run(t.Context())
	if !errors.Is(err, crypto.ErrUnavailable) || res.Rewrapped != 0 {
		t.Fatalf("Run during an outage = %+v, %v; want ErrUnavailable and nothing rewrapped", res, err)
	}
	for id, v := range before {
		if !bytes.Equal(f.value(t, id), v) {
			t.Fatalf("row %s changed during an outage", id)
		}
	}
}
