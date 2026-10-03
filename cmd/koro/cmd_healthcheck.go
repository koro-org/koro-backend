package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"koro/internal/platform/config"
)

// runHealthcheck runs `koro healthcheck`, the Docker HEALTHCHECK of the
// distroless image (no curl/wget there): GET /healthz on the local API
// (HTTP_ADDR) must answer 200 within the timeout.
func runHealthcheck(ctx context.Context, args []string, environ map[string]string, stderr io.Writer) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("url", "", "health URL (default: http://127.0.0.1:<HTTP_ADDR port>/healthz)")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if *target == "" {
		cfg, err := config.LoadHealthcheck(environ)
		if err != nil {
			return err
		}
		if *target, err = healthURL(cfg.HTTPAddr); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *target, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	client := &http.Client{Timeout: *timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("unhealthy: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s answered %d", *target, resp.StatusCode)
	}
	return nil
}

// healthURL maps a listen address (":8080", "0.0.0.0:8080") to a loopback URL.
func healthURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("HTTP_ADDR %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
