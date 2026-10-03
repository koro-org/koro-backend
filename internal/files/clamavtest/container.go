//go:build integration

package clamavtest

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: ClamAV hash signatures (.hdb) are MD5 by format; test data only
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"koro/internal/platform/testutil"
)

// Real clamd for integration tests. The image is the ClamAV release of
// deploy/compose.dev.yaml in its "_base" flavour (no bundled signature
// database, ~75 MB): instead of the ~300 MB official database (≈1.5 GB of
// memory, a minute to load) clamd loads two EICAR signatures written here,
// so it starts in seconds with ~15 MB. The engine, the protocol and the
// verdict path are the production ones. One container serves a whole test
// binary (Main), like storagetest.
const (
	Image = "clamav/clamav:1.5.4_base-debian"

	// HashSignature matches the exact EICAR file (as the official
	// Win.Test.EICAR_HDB-1 does); BodySignature matches the EICAR string
	// anywhere in a file (an upload must be a valid PDF or image, so the
	// pipeline tests embed it). clamd appends ".UNOFFICIAL" to both.
	HashSignature = "Koro.Test.EICAR-HDB"
	BodySignature = "Koro.Test.EICAR-Body"

	// StreamMaxLength is clamd's limit in the container (production value).
	StreamMaxLength = 25 << 20

	setupTimeout = 5 * time.Minute
	readyTimeout = 3 * time.Minute
)

var startBackoff = testutil.Backoff{Attempts: 3, Base: 2 * time.Second, Max: 10 * time.Second}

var (
	once   sync.Once
	done   = make(chan struct{})
	mu     sync.Mutex
	ctr    *testcontainers.DockerContainer
	addr   string
	errRun error
)

// Main starts the shared clamd in the background, runs the tests with run
// and terminates the container:
//
//	func TestMain(m *testing.M) { os.Exit(clamavtest.Main(m, (*testing.M).Run)) }
func Main(m *testing.M, run func(*testing.M) int) int {
	go launch()
	code := run(m)
	<-done
	mu.Lock()
	c := ctr
	mu.Unlock()
	if c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute) //nolint:forbidigo // TestMain teardown has no test context
		defer cancel()
		if err := c.Terminate(ctx, testcontainers.StopTimeout(time.Second)); err != nil {
			fmt.Fprintf(os.Stderr, "clamavtest: terminate clamd: %v\n", err)
		}
	}
	return code
}

func launch() {
	once.Do(func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout) //nolint:forbidigo // shared set-up outlives every test context
		defer cancel()
		addr, errRun = start(ctx)
		if errRun != nil {
			fmt.Fprintf(os.Stderr, "clamavtest: %v\n", errRun)
		}
	})
}

// Start returns host:port of the shared clamd, waiting until it answers.
func Start(t testing.TB) string {
	t.Helper()
	go launch()
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatalf("clamavtest: test ended while waiting for clamd: %v", t.Context().Err())
	}
	if errRun != nil {
		t.Fatalf("clamavtest: %v", errRun)
	}
	return addr
}

// signatures returns the test database: an MD5 (hdb) and a body (ndb)
// signature of EICAR.
func signatures() (hdb, ndb string) {
	sum := md5Hex(EICAR)
	hdb = fmt.Sprintf("%s:%d:%s\n", sum, len(EICAR), HashSignature)
	ndb = fmt.Sprintf("%s:0:*:%s\n", BodySignature, hex.EncodeToString(EICAR))
	return hdb, ndb
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // G401: ClamAV .hdb format, not a security use
	return hex.EncodeToString(sum[:])
}

func start(ctx context.Context) (string, error) {
	hdb, ndb := signatures()
	var out string
	err := testutil.Retry(ctx, startBackoff, "start clamd", nil, func(ctx context.Context) error {
		c, err := testcontainers.Run(ctx, Image,
			// A leading "-" makes the entrypoint exec clamd itself: no
			// freshclam download, no daemon.
			testcontainers.WithCmd("--foreground"),
			testcontainers.WithEnv(map[string]string{"CLAMD_CONF_StreamMaxLength": "25M"}),
			testcontainers.WithFiles(
				testcontainers.ContainerFile{Reader: strings.NewReader(hdb), ContainerFilePath: "/var/lib/clamav/koro-test.hdb", FileMode: 0o644},
				testcontainers.ContainerFile{Reader: strings.NewReader(ndb), ContainerFilePath: "/var/lib/clamav/koro-test.ndb", FileMode: 0o644},
			),
			testcontainers.WithExposedPorts("3310/tcp"),
			testcontainers.WithWaitStrategy(wait.ForListeningPort("3310/tcp").WithStartupTimeout(readyTimeout)),
		)
		if err != nil {
			if c != nil {
				_ = c.Terminate(context.WithoutCancel(ctx))
			}
			return fmt.Errorf("run container: %w", err)
		}
		ep, err := c.PortEndpoint(ctx, "3310/tcp", "")
		if err == nil {
			err = pong(ctx, ep)
		}
		if err != nil {
			_ = c.Terminate(context.WithoutCancel(ctx))
			return err
		}
		mu.Lock()
		ctr = c
		mu.Unlock()
		out = ep
		return nil
	})
	return out, err
}

// pong waits until clamd answers PING (the port opens before the engine
// has loaded the database).
func pong(ctx context.Context, ep string) error {
	return testutil.Retry(ctx, testutil.Backoff{Attempts: 60, Base: 250 * time.Millisecond, Max: 2 * time.Second},
		"clamd ping", nil, func(ctx context.Context) error {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", ep)
			if err != nil {
				return err
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:forbidigo // network deadline in a test helper
			if _, err := conn.Write([]byte("zPING\x00")); err != nil {
				return err
			}
			b := make([]byte, 16)
			n, err := conn.Read(b)
			if err != nil {
				return err
			}
			if got := strings.TrimRight(string(b[:n]), "\x00"); got != "PONG" {
				return fmt.Errorf("clamd answered %q", got)
			}
			return nil
		})
}
