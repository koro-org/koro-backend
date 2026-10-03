package httpx_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

func TestHTTP_GracefulShutdown_DrainsInFlight(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	baseCtx, hardStop := context.WithCancel(context.Background())
	defer hardStop()
	srv := httpx.NewServer(mux, baseCtx, obs.Discard())
	shutdownStarted := make(chan struct{})
	srv.RegisterOnShutdown(func() { close(shutdownStarted) })

	ready := httpx.NewReady()
	sigCtx, signal := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpx.Serve(sigCtx, srv, ln, httpx.ServeOptions{
			Stop:            func() { close(stopped) },
			Ready:           ready,
			DrainDelay:      10 * time.Millisecond,
			ShutdownTimeout: 5 * time.Second,
			HardStop:        hardStop,
			Log:             obs.Discard(),
		})
	}()

	type result struct {
		status int
		body   string
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/slow", nil)
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := client.Do(req)
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		resCh <- result{status: resp.StatusCode, body: string(b), err: err}
	}()

	<-started
	if !ready.IsReady() {
		t.Fatal("Ready must be true while serving")
	}
	signal() // SIGTERM

	<-shutdownStarted // srv.Shutdown is running while the request is still in flight
	<-stopped
	if ready.IsReady() {
		t.Fatal("Ready must be false once shutdown starts (/readyz 503)")
	}
	select {
	case err := <-serveErr:
		t.Fatalf("Serve returned before draining the in-flight request: %v", err)
	default:
	}

	close(release)
	res := <-resCh
	if res.err != nil || res.status != http.StatusOK || res.body != "done" {
		t.Fatalf("in-flight request = %d %q %v, want 200 done", res.status, res.body, res.err)
	}
	if err := <-serveErr; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", addr); err == nil {
		_ = conn.Close()
		t.Fatal("listener still accepts connections after shutdown")
	}
}
