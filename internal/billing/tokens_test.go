package billing_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"koro/internal/billing"
	"koro/internal/platform/db"
)

// serialTx is db.NopTx whose outermost transactions run one at a time: in
// memory it stands for PostgreSQL's row lock, which makes two transactions
// locking the same integration_credentials row wait for each other
// (TestParasut_TokenRefresh_RowLock_SingleRefresher_Postgres proves the
// lock itself on the GORM store).
type serialTx struct{ mu sync.Mutex }

func (s *serialTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if db.InTx(ctx) {
		return fn(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return db.NopTx{}.WithTx(ctx, fn)
}

func (s *serialTx) WithSerializableTx(ctx context.Context, fn func(context.Context) error) error {
	return s.WithTx(ctx, fn)
}

// TestParasut_TokenRefresh_RowLock_SingleRefresher: workers that see the
// same token refused (a 401 each) refresh it once — the first takes the
// row lock and rotates, the others wait for the lock, read the renewed row
// and use it; the same holds for concurrent runs of billing.refresh_token.
// Paraşüt rotates the refresh token, so a second refresher would present a
// dead one (invalid_grant) and lose the integration.
func TestParasut_TokenRefresh_RowLock_SingleRefresher(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newParasutRig(t, &serialTx{}, nil)
	stale, err := r.tokens.Token(ctx)
	if err != nil || stale.Access != "dbl-access-0" || stale.Account != dblCompany {
		t.Fatalf("Token = %v, %v", stale, err)
	}

	const workers = 8
	got := make([]billing.ProviderToken, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() { got[i], errs[i] = r.tokens.Refresh(ctx, stale.Access) })
	}
	wg.Wait()
	for i := range workers {
		if errs[i] != nil || got[i].Access == stale.Access || got[i].Access != got[0].Access || got[i].Account != dblCompany {
			t.Fatalf("worker %d: %v, %v; want the one renewed token", i, got[i], errs[i])
		}
	}
	if n := r.d.tokenCount(); n != 1 {
		t.Fatalf("token endpoint called %d times, want 1", n)
	}
	if n := len(r.audit.Of("billing.integration.refresh")); n != 1 {
		t.Fatalf("refresh audit records = %d, want 1", n)
	}
	c, err := r.store.Credential(ctx, billing.ProviderParasut, false)
	if err != nil || c.ExpiresAt == nil || !c.ExpiresAt.Equal(t0.Add(2*time.Hour)) || c.RotatedAt == nil {
		t.Fatalf("credential after the refresh = %+v, %v", c, err)
	}

	// The scheduled rotation, three replicas at once, an hour before expiry.
	r.clk.Advance(61 * time.Minute)
	for range 3 {
		wg.Go(func() {
			if err := r.tokens.RefreshDue(ctx); err != nil {
				t.Errorf("RefreshDue: %v", err)
			}
		})
	}
	wg.Wait()
	if n := r.d.tokenCount(); n != 2 {
		t.Fatalf("token endpoint called %d times after the job, want 2", n)
	}
	now, err := r.tokens.Token(ctx)
	if err != nil || now.Access == got[0].Access {
		t.Fatalf("Token after the job = %v, %v; want the rotated token", now, err)
	}
	if _, err := r.provider.UpsertContact(ctx, contractParties[0]); err != nil {
		t.Fatalf("a call with the rotated token: %v", err)
	}
}

// TestProviderTokens_RefreshDue: no credentials or no refresh token is
// nothing to do; a token far from its expiry is kept; one about to expire
// is renewed before use; a refused refresh token is ErrRefreshRefused and
// logged as an alarm.
func TestProviderTokens_RefreshDue(t *testing.T) {
	t.Parallel()

	t.Run("no credentials", func(t *testing.T) {
		t.Parallel()
		r := newParasutRig(t, nil, billing.NewMemStore(func() time.Time { return t0 }))
		ctx := t.Context()
		empty := billing.NewMemStore(func() time.Time { return t0 })
		tokens, err := billing.NewProviderTokens(billing.TokenDeps{Provider: billing.ProviderParasut, Store: empty, Tx: db.NopTx{},
			Sealer: testEnvelope(t), Refresher: refresherFunc(func(context.Context, string) (billing.TokenGrant, error) {
				t.Fatal("no refresh expected")
				return billing.TokenGrant{}, nil
			}), Clock: r.clk, Log: r.log, Audit: r.audit})
		if err != nil {
			t.Fatal(err)
		}
		if err := tokens.RefreshDue(ctx); err != nil {
			t.Fatalf("RefreshDue without credentials = %v", err)
		}
		if _, err := tokens.Token(ctx); !errors.Is(err, billing.ErrNoCredentials) {
			t.Fatalf("Token without credentials = %v, want ErrNoCredentials", err)
		}
	})

	t.Run("fresh, due, expired, refused", func(t *testing.T) {
		t.Parallel()
		r := newParasutRig(t, nil, nil)
		ctx := t.Context()
		if err := r.tokens.RefreshDue(ctx); err != nil || r.d.tokenCount() != 0 {
			t.Fatalf("RefreshDue(2 h left) = %v, calls %d; want kept", err, r.d.tokenCount())
		}
		r.clk.Advance(2 * time.Hour) // expired: renewed before use
		if tok, err := r.tokens.Token(ctx); err != nil || tok.Access == "dbl-access-0" || r.d.tokenCount() != 1 {
			t.Fatalf("Token(expired) = %v, %v; calls %d", tok, err, r.d.tokenCount())
		}
		r.d.set(func(d *parasutDouble) { d.refresh = "dbl-refresh-elsewhere" })
		r.clk.Advance(90 * time.Minute)
		err := r.tokens.RefreshDue(ctx)
		if !errors.Is(err, billing.ErrRefreshRefused) {
			t.Fatalf("RefreshDue(refused) = %v, want ErrRefreshRefused", err)
		}
		if logs := r.logs.String(); !strings.Contains(logs, `"alarm":true`) || !strings.Contains(logs, "integration token not renewed") {
			t.Fatalf("no alarm logged:\n%s", logs)
		}
	})
}

type refresherFunc func(context.Context, string) (billing.TokenGrant, error)

func (f refresherFunc) Refresh(ctx context.Context, rt string) (billing.TokenGrant, error) {
	return f(ctx, rt)
}
