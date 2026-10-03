//go:build integration

// The container helper is built only with the integration tag: it pulls in
// testcontainers (and its tar/archive dependencies), which must stay out of
// the source govulncheck scan of the production build graph.

package openbaotest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"koro/internal/platform/testutil"
)

// Image is the OpenBao image of the integration tests and of
// deploy/compose.dev.yaml.
const Image = "openbao/openbao:2.7.0"

// AppPolicy is the minimal policy of the Koro token (package openbao doc).
const AppPolicy = `
path "transit/encrypt/%[1]s" { capabilities = ["update"] }
path "transit/decrypt/%[1]s" { capabilities = ["update"] }
path "transit/keys/%[1]s"    { capabilities = ["read"] }
`

// Server is a running OpenBao dev server with Transit enabled and one key.
type Server struct {
	Addr      string // http://host:port
	RootToken string // admin operations of the test (rotate)
	AppToken  string // a periodic token with AppPolicy only
	Key       string
}

const rootToken = "koro-test-root" //nolint:gosec // G101: dev-mode root token of a throwaway container

// Start runs OpenBao in dev mode (in-memory, unsealed), enables Transit,
// creates an aes256-gcm96 key and a periodic token restricted to
// AppPolicy. The container is removed when the test ends.
func Start(t testing.TB, key string) Server {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	var ctr *testcontainers.DockerContainer
	backoff := testutil.Backoff{Attempts: 4, Base: 500 * time.Millisecond, Max: 8 * time.Second}
	err := testutil.Retry(ctx, backoff, "start openbao", nil, func(ctx context.Context) error {
		c, err := testcontainers.Run(ctx, Image,
			testcontainers.WithExposedPorts("8200/tcp"),
			testcontainers.WithEnv(map[string]string{
				"BAO_DEV_ROOT_TOKEN_ID":  rootToken,
				"BAO_DEV_LISTEN_ADDRESS": "0.0.0.0:8200",
			}),
			testcontainers.WithCmd("server", "-dev"),
			testcontainers.WithWaitStrategy(
				wait.ForHTTP("/v1/sys/health").WithPort("8200/tcp").WithStartupTimeout(2*time.Minute),
			),
		)
		if err != nil {
			if c != nil {
				_ = c.Terminate(context.WithoutCancel(ctx))
			}
			return err
		}
		ctr = c
		return nil
	})
	if err != nil {
		t.Fatalf("openbaotest: %v", err)
	}
	stop := context.WithoutCancel(t.Context())
	t.Cleanup(func() { _ = ctr.Terminate(stop) })
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(ctx, "8200/tcp")
	if err != nil {
		t.Fatal(err)
	}
	s := Server{Addr: fmt.Sprintf("http://%s:%d", host, port.Num()), RootToken: rootToken, Key: key}
	s.Admin(t, http.MethodPost, "sys/mounts/transit", map[string]any{"type": "transit"}, nil)
	s.Admin(t, http.MethodPost, "transit/keys/"+key, map[string]any{"type": "aes256-gcm96"}, nil)
	s.Admin(t, http.MethodPost, "sys/policies/acl/koro-app", map[string]any{"policy": fmt.Sprintf(AppPolicy, key)}, nil)
	var tok struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	s.Admin(t, http.MethodPost, "auth/token/create-orphan", map[string]any{"policies": []string{"koro-app"}, "period": "72h"}, &tok)
	if tok.Auth.ClientToken == "" {
		t.Fatal("openbaotest: no app token")
	}
	s.AppToken = tok.Auth.ClientToken
	return s
}

// Rotate adds a version to the key.
func (s Server) Rotate(t testing.TB) {
	t.Helper()
	s.Admin(t, http.MethodPost, "transit/keys/"+s.Key+"/rotate", map[string]any{}, nil)
}

// Admin performs one request with the root token and fails the test on a
// non-2xx answer.
func (s Server) Admin(t testing.TB, method, path string, in, out any) {
	t.Helper()
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(t.Context(), method, s.Addr+"/v1/"+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Vault-Token", s.RootToken)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("openbaotest %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("openbaotest %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("openbaotest %s %s: %v", method, path, err)
		}
	}
}
