package consent_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"koro/internal/consent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/iys"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// pushRecorder is a pushing İYS adapter's receiver that remembers the
// deliveries and answers err.
type pushRecorder struct {
	mu   sync.Mutex
	got  []iys.Delivery
	err  error
	name string
}

func (p *pushRecorder) Name() string { return p.name }

func (p *pushRecorder) Receive(_ context.Context, d iys.Delivery) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, d)
	return 1, p.err
}

// TestIYSWebhook_RoutesToTheReceiver (BE-110): receiveIYSWebhook hands the
// delivery of the configured, pushing adapter to it with the client
// address the API resolved; another provider, or no pushing adapter, is
// 404; the adapter's answers map to 401 (not authenticated), 422 (not its
// format) and 503 (try again).
func TestIYSWebhook_RoutesToTheReceiver(t *testing.T) {
	t.Parallel()
	ctx := httpx.WithClientIP(t.Context(), "192.0.2.10")
	body := []byte(`[{"type":"EPOSTA"}]`)

	none := newEnv(t)
	if err := none.svc.ReceiveIYSWebhook(ctx, "netgsm", "token", body); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("without a pushing adapter = %v, want 404", err)
	}

	rec := &pushRecorder{name: "netgsm"}
	e := newEnv(t, func(d *consent.Deps) { d.IYSPush = rec })
	if err := e.svc.ReceiveIYSWebhook(ctx, "verimor", "token", body); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("another provider = %v, want 404", err)
	}
	if err := e.svc.ReceiveIYSWebhook(ctx, "netgsm", "the-token", body); err != nil {
		t.Fatalf("delivery = %v", err)
	}
	if len(rec.got) != 1 || rec.got[0].Token != "the-token" || rec.got[0].ClientIP != "192.0.2.10" || string(rec.got[0].Body) != string(body) {
		t.Fatalf("receiver got %+v", rec.got)
	}
	for _, tc := range []struct {
		err    error
		status int
	}{
		{iys.ErrUnauthenticated, http.StatusUnauthorized},
		{errors.Join(iys.ErrRejected, errors.New("malformed item 2")), http.StatusUnprocessableEntity},
		{errors.New("store unreachable"), http.StatusServiceUnavailable},
	} {
		rec.mu.Lock()
		rec.err = tc.err
		rec.mu.Unlock()
		err := e.svc.ReceiveIYSWebhook(ctx, "netgsm", "the-token", body)
		if status, _ := problemtest.StatusCode(err); status != tc.status {
			t.Fatalf("receiver error %v → %v, want HTTP %d", tc.err, err, tc.status)
		}
	}
}
