//go:build integration

// Package storagetest starts a SeaweedFS S3 server (the local object storage
// of deploy/compose.dev.yaml) for integration tests (05 §14.7: the S3 client
// is tested against the target storage). One container serves the whole
// test binary: Main starts it once per package, in the background while run
// prepares the other containers, and terminates it at the end.
//
//	func TestMain(m *testing.M) { os.Exit(storagetest.Main(m, testutil.Run)) }
//	...
//	s3 := storagetest.Client(t)
//	bucket := storagetest.Bucket(t, s3)
//
// Start-up is hardened for a loaded Docker host (06 H-36, BE-059): the
// reaper timeout is raised, a start that fails or times out is terminated
// and retried with backoff, the container is "ready" only once S3 accepts a
// bucket creation (SeaweedFS answers /healthz before its first volume can
// take writes), and bucket creation itself is retried.
package storagetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"koro/internal/platform/clock"
	"koro/internal/platform/storage"
	"koro/internal/platform/testutil"
)

// Settings of the test server (test-only credentials).
const (
	Image     = "chrislusf/seaweedfs:4.48" // deploy/compose.dev.yaml
	AccessKey = "koro-test-access-key"
	SecretKey = "koro-test-secret-key" // test-only credential of a throwaway container
	Region    = "us-east-1"

	s3Config = `{"identities":[{"name":"koro-test","credentials":[{"accessKey":"` + AccessKey +
		`","secretKey":"` + SecretKey + `"}],"actions":["Admin","Read","List","Tagging","Write"]}]}`

	// setupTimeout bounds the whole start-up, retries included.
	setupTimeout = 8 * time.Minute
	// readyTimeout bounds one container start until /healthz answers (the
	// testcontainers default minute timed out under the full suite).
	readyTimeout = 3 * time.Minute
	// writableTimeout bounds the wait until S3 accepts the first bucket.
	writableTimeout = 2 * time.Minute
	// ryukTimeout is the reaper connection timeout (default 1 m), set
	// through the testcontainers environment unless the caller chose one
	// (testutil.Run does the same, but may run after the start began).
	ryukTimeout = "3m"
	// probeBucket is created once to prove S3 accepts writes.
	probeBucket = "koro-test-ready"
)

// startBackoff retries a failed container start: three tries, a few
// seconds apart (each try may itself wait readyTimeout).
var startBackoff = testutil.Backoff{Attempts: 3, Base: 2 * time.Second, Max: 10 * time.Second}

// Server is a running SeaweedFS S3 endpoint.
type Server struct {
	Endpoint string // http://host:port
}

var (
	once   sync.Once
	done   = make(chan struct{}) // closed when the start-up finished
	mu     sync.Mutex
	ctr    *testcontainers.DockerContainer
	server *Server
	errRun error
)

// Main starts the shared SeaweedFS container in the background, runs the
// tests with run (testutil.Run, or (*testing.M).Run) and terminates the
// container. A test that needs S3 waits in Start until it is ready.
func Main(m *testing.M, run func(*testing.M) int) int {
	for _, k := range []string{"TESTCONTAINERS_RYUK_CONNECTION_TIMEOUT", "TESTCONTAINERS_RYUK_RECONNECTION_TIMEOUT"} {
		if _, set := os.LookupEnv(k); !set { //nolint:forbidigo // testcontainers is configured through its environment (as testutil.Run)
			_ = os.Setenv(k, ryukTimeout)
		}
	}
	go launch()
	code := run(m)
	<-done // never terminate while a start is in flight
	mu.Lock()
	c := ctr
	mu.Unlock()
	if c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute) //nolint:forbidigo // TestMain teardown has no test context
		defer cancel()
		if err := c.Terminate(ctx, testcontainers.StopTimeout(time.Second)); err != nil {
			fmt.Fprintf(os.Stderr, "storagetest: terminate seaweedfs: %v\n", err)
		}
	}
	return code
}

// launch starts the server exactly once (from Main, or from the first
// Start of a binary without Main).
func launch() {
	once.Do(func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout) //nolint:forbidigo // shared set-up outlives every test context
		defer cancel()
		server, errRun = start(ctx)
		if errRun != nil {
			fmt.Fprintf(os.Stderr, "storagetest: %v\n", errRun)
		}
	})
}

// Start returns the shared server, waiting until it is ready.
func Start(t testing.TB) *Server {
	t.Helper()
	go launch()
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatalf("storagetest: test ended while waiting for seaweedfs: %v", t.Context().Err())
	}
	if errRun != nil {
		t.Fatalf("storagetest: %v", errRun)
	}
	return server
}

// start runs the container (retrying a failed start) and waits until S3
// accepts writes.
func start(ctx context.Context) (*Server, error) {
	var srv *Server
	err := testutil.Retry(ctx, startBackoff, "start seaweedfs", nil, func(ctx context.Context) error {
		c, err := testcontainers.Run(ctx, Image,
			testcontainers.WithCmd("server", "-dir=/data", "-ip.bind=0.0.0.0", "-master.volumeSizeLimitMB=64",
				"-volume.max=0", "-s3", "-s3.port=8333", "-s3.config=/etc/seaweedfs/s3.json"),
			testcontainers.WithFiles(testcontainers.ContainerFile{
				Reader: strings.NewReader(s3Config), ContainerFilePath: "/etc/seaweedfs/s3.json", FileMode: 0o644,
			}),
			testcontainers.WithExposedPorts("8333/tcp"),
			testcontainers.WithWaitStrategy(wait.ForHTTP("/healthz").WithPort("8333/tcp").
				WithStartupTimeout(readyTimeout).WithPollInterval(500*time.Millisecond)),
		)
		if err != nil {
			if c != nil {
				_ = c.Terminate(context.WithoutCancel(ctx))
			}
			return fmt.Errorf("run container: %w", err)
		}
		ep, err := c.PortEndpoint(ctx, "8333/tcp", "http")
		if err == nil {
			err = writable(ctx, ep)
		}
		if err != nil {
			_ = c.Terminate(context.WithoutCancel(ctx))
			return err
		}
		mu.Lock()
		ctr = c
		mu.Unlock()
		srv = &Server{Endpoint: ep}
		return nil
	})
	return srv, err
}

// writable waits until the server creates a bucket (its first volume is
// allocated lazily, after /healthz is already up).
func writable(ctx context.Context, endpoint string) error {
	s3, err := newClient(endpoint)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writableTimeout)
	defer cancel()
	return testutil.Retry(ctx, testutil.Backoff{Attempts: 40, Base: 200 * time.Millisecond, Max: 5 * time.Second},
		"seaweedfs first bucket", nil, func(ctx context.Context) error { return s3.CreateBucket(ctx, probeBucket) })
}

func newClient(endpoint string) (*storage.S3, error) {
	return storage.NewS3(storage.Config{
		Endpoint: endpoint, Region: Region, AccessKey: AccessKey, SecretKey: SecretKey,
		PathStyle: true, Timeout: time.Minute, Clock: clock.Real(),
	})
}

// Client returns an S3 client of the shared server.
func Client(t testing.TB) *storage.S3 {
	t.Helper()
	s3, err := newClient(Start(t).Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return s3
}

// Bucket creates a fresh private bucket with a random name. Creation is
// retried: a loaded server may refuse or time out for a moment.
func Bucket(t testing.TB, s3 *storage.S3) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "koro-test-" + hex.EncodeToString(b)
	err := testutil.Retry(t.Context(), testutil.Backoff{Attempts: 10, Base: 250 * time.Millisecond, Max: 5 * time.Second},
		"create bucket "+name, nil, func(ctx context.Context) error { return s3.CreateBucket(ctx, name) })
	if err != nil {
		t.Fatalf("storagetest: %v", err)
	}
	return name
}
