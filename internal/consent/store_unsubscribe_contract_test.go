package consent_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strconv"
	"testing"
	"time"
	"uuid"

	"koro/internal/consent"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// unsubscribeLinkContract is the contract of UnsubscribeLinkStore both
// stores pass (05 §13.3; BE-153): the in-memory one here, the GORM one
// against PostgreSQL in store_unsubscribe_integration_test.go.
func unsubscribeLinkContract(t *testing.T, newWorld func(t *testing.T) storeWorld) {
	t.Helper()
	base := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	digest := func(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }
	link := func(user uuid.UUID, n int, expires time.Time) consent.UnsubscribeToken {
		return consent.UnsubscribeToken{ID: uuid.NewV7(), TokenHash: digest("token-" + strconv.Itoa(n) + "-" + user.String()), UserID: user,
			Purpose: consent.PurposeCommercialEmail, CreatedAt: base, ExpiresAt: expires}
	}

	t.Run("insert and look up by digest", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		u := w.user(t)
		l := link(u, 1, base.Add(365*24*time.Hour))
		if err := w.s.InsertUnsubscribeLink(ctx, l); err != nil {
			t.Fatal(err)
		}
		got, err := w.s.UnsubscribeLinkByHash(ctx, bytes.Clone(l.TokenHash))
		if err != nil || got.ID != l.ID || got.UserID != u || got.Purpose != consent.PurposeCommercialEmail ||
			!bytes.Equal(got.TokenHash, l.TokenHash) || !got.CreatedAt.Equal(base) || !got.ExpiresAt.Equal(l.ExpiresAt) {
			t.Fatalf("UnsubscribeLinkByHash = %+v, %v; want %+v", got, err, l)
		}
		if _, err := w.s.UnsubscribeLinkByHash(ctx, digest("never minted")); !errors.Is(err, consent.ErrNotFound) {
			t.Fatalf("unknown digest = %v, want ErrNotFound", err)
		}
		dup := link(u, 2, l.ExpiresAt)
		dup.TokenHash = l.TokenHash
		if err := w.s.InsertUnsubscribeLink(ctx, dup); !errors.Is(err, problem.ErrConflict) {
			t.Fatalf("a digest twice = %v, want 409 (unsubscribe_links_token_hash_uq)", err)
		}
		with := func(f func(*consent.UnsubscribeToken)) consent.UnsubscribeToken {
			l := link(u, 3, base.Add(time.Hour))
			f(&l)
			return l
		}
		for name, bad := range map[string]consent.UnsubscribeToken{
			"short digest":     with(func(l *consent.UnsubscribeToken) { l.TokenHash = []byte("short") }),
			"push purpose":     with(func(l *consent.UnsubscribeToken) { l.Purpose = consent.PurposeCommercialPush }),
			"expiry not later": with(func(l *consent.UnsubscribeToken) { l.ExpiresAt = l.CreatedAt }),
		} {
			if err := w.s.InsertUnsubscribeLink(ctx, bad); !errors.Is(err, problem.ErrValidation) {
				t.Errorf("%s = %v, want 422", name, err)
			}
		}
	})

	t.Run("delete by user and by expiry", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		u, other := w.user(t), w.user(t)
		early, late, live := link(u, 1, base.Add(time.Hour)), link(other, 2, base.Add(2*time.Hour)), link(other, 3, base.Add(48*time.Hour))
		mine := link(u, 4, base.Add(48*time.Hour))
		for _, l := range []consent.UnsubscribeToken{live, late, mine, early} {
			if err := w.s.InsertUnsubscribeLink(ctx, l); err != nil {
				t.Fatal(err)
			}
		}
		has := func(l consent.UnsubscribeToken) bool {
			_, err := w.s.UnsubscribeLinkByHash(ctx, l.TokenHash)
			if err != nil && !errors.Is(err, consent.ErrNotFound) {
				t.Fatal(err)
			}
			return err == nil
		}
		// Oldest expiry first, at most limit per call; nothing at the cut-off.
		if n, err := w.s.DeleteExpiredUnsubscribeLinks(ctx, base.Add(3*time.Hour), 1); err != nil || n != 1 || has(early) || !has(late) {
			t.Fatalf("first batch = %d, %v (early kept %v, late kept %v)", n, err, has(early), has(late))
		}
		if n, err := w.s.DeleteExpiredUnsubscribeLinks(ctx, late.ExpiresAt, 10); err != nil || n != 0 || !has(late) {
			t.Fatalf("expiry equal to the cut-off = %d, %v", n, err)
		}
		if n, err := w.s.DeleteExpiredUnsubscribeLinks(ctx, base.Add(3*time.Hour), 10); err != nil || n != 1 || has(late) || !has(live) {
			t.Fatalf("second batch = %d, %v", n, err)
		}
		// The erasure: every link of the user, nobody else's.
		if n, err := w.s.DeleteUnsubscribeLinksOf(ctx, u); err != nil || n != 1 || has(mine) || !has(live) {
			t.Fatalf("DeleteUnsubscribeLinksOf = %d, %v", n, err)
		}
		if n, err := w.s.DeleteUnsubscribeLinksOf(ctx, u); err != nil || n != 0 {
			t.Fatalf("second DeleteUnsubscribeLinksOf = %d, %v", n, err)
		}
	})
}

func TestUnsubscribeLinkStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	unsubscribeLinkContract(t, func(*testing.T) storeWorld {
		return storeWorld{
			s: consent.NewMemStore(time.Now), tx: db.NopTx{},
			user: func(*testing.T) uuid.UUID { return uuid.NewV7() },
		}
	})
}
