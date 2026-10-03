//go:build integration

// Package storagetest starts a SeaweedFS S3 server (the local object storage
// of deploy/compose.dev.yaml) for integration tests (05 §14.7: the S3 client
// is tested against the target storage). One container is shared by the
// test binary and started on first use; Main terminates it.
//
//	func TestMain(m *testing.M) { os.Exit(storagetest.Main(m, testutil.Run)) }
//	...
//	s3 := storagetest.Client(t)
//	bucket := storagetest.Bucket(t, s3)
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
)

// Settings of the test server (test-only credentials).
const (
	Image     = "chrislusf/seaweedfs:4.48" // deploy/compose.dev.yaml
	AccessKey = "koro-test-access-key"
	SecretKey = "koro-test-secret-key" // test-only credential of a throwaway container
	Region    = "us-east-1"

	s3Config = `{"identities":[{"name":"koro-test","credentials":[{"accessKey":"` + AccessKey +
		`","secretKey":"` + SecretKey + `"}],"actions":["Admin","Read","List","Tagging","Write"]}]}`
	startTimeout = 3 * time.Minute
)

// Server is a running SeaweedFS S3 endpoint.
type Server struct {
	Endpoint string // http://host:port
}

var (
	once   sync.Once
	ctr    *testcontainers.DockerContainer
	server *Server
	errRun error
)

// Main runs the tests with run (testutil.Run, or m.Run) and terminates the
// SeaweedFS container if a test started it.
func Main(m *testing.M, run func(*testing.M) int) int {
	code := run(m)
	if ctr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute) //nolint:forbidigo // TestMain teardown has no test context
		defer cancel()
		if err := ctr.Terminate(ctx, testcontainers.StopTimeout(time.Second)); err != nil {
			fmt.Fprintf(os.Stderr, "storagetest: terminate seaweedfs: %v\n", err)
		}
	}
	return code
}

// Start returns the shared server, starting it on first use.
func Start(t testing.TB) *Server {
	t.Helper()
	once.Do(func() {
		// The container outlives the first test: detach from its context.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), startTimeout)
		defer cancel()
		server, errRun = start(ctx)
	})
	if errRun != nil {
		t.Fatalf("storagetest: %v", errRun)
	}
	return server
}

func start(ctx context.Context) (*Server, error) {
	c, err := testcontainers.Run(ctx, Image,
		testcontainers.WithCmd("server", "-dir=/data", "-ip.bind=0.0.0.0", "-master.volumeSizeLimitMB=64",
			"-volume.max=0", "-s3", "-s3.port=8333", "-s3.config=/etc/seaweedfs/s3.json"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader: strings.NewReader(s3Config), ContainerFilePath: "/etc/seaweedfs/s3.json", FileMode: 0o644,
		}),
		testcontainers.WithExposedPorts("8333/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/healthz").WithPort("8333/tcp").WithStartupTimeout(startTimeout)),
	)
	ctr = c
	if err != nil {
		return nil, fmt.Errorf("start seaweedfs: %w", err)
	}
	ep, err := c.PortEndpoint(ctx, "8333/tcp", "http")
	if err != nil {
		return nil, fmt.Errorf("seaweedfs endpoint: %w", err)
	}
	return &Server{Endpoint: ep}, nil
}

// Client returns an S3 client of the shared server.
func Client(t testing.TB) *storage.S3 {
	t.Helper()
	srv := Start(t)
	s3, err := storage.NewS3(storage.Config{
		Endpoint: srv.Endpoint, Region: Region, AccessKey: AccessKey, SecretKey: SecretKey,
		PathStyle: true, Clock: clock.Real(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s3
}

// Bucket creates a fresh private bucket with a random name. SeaweedFS may
// still be growing its first volumes right after start: creation is retried.
func Bucket(t testing.TB, s3 *storage.S3) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "koro-test-" + hex.EncodeToString(b)
	ctx := t.Context()
	var err error
	for attempt := range 30 {
		if err = s3.CreateBucket(ctx, name); err == nil {
			return name
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	t.Fatalf("storagetest: create bucket %s: %v", name, err)
	return ""
}
