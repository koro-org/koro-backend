package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Files configures the files module (internal/files). Owner: BE-008, then
// BE-048 (ClamAV), BE-113 (adapter selection); only tickets of the files module edit this file and the
// "# --- files ---" block of .env.example (BE-001 R7).
//
// Development defaults run the no-op scanner (scan_status "skipped") so that
// `make dev` and the tests need no ClamAV. Staging and prod process real
// CVs and documents (T-81): there FILES_SCANNER must be clamav and
// FILES_SCAN_REQUIRED true, or the process does not start.
type Files struct {
	// ScanRequired makes downloads require scan_status=clean. While false
	// a file whose scan was skipped is downloadable too. true needs a real
	// scanner: the wiring refuses to start with the no-op scanner.
	ScanRequired bool `env:"FILES_SCAN_REQUIRED" envDefault:"false"`
	// UploadURLTTL is the lifetime of a presigned upload URL (04 §1.14:
	// 10 minutes; at most storage.MaxPutTTL).
	UploadURLTTL time.Duration `env:"FILES_UPLOAD_URL_TTL" envDefault:"10m"`

	// Scanner selects the files.Scanner adapter of the upload pipeline
	// (the port's <SECTION>_PROVIDER key; the name predates BE-113): "noop"
	// (marks every file skipped; development only) or "clamav" (clamd over
	// TCP, INSTREAM).
	Scanner string `env:"FILES_SCANNER" envDefault:"noop"`
	// ClamAVAddr is clamd's TCPSocket as host:port (deploy/compose.dev.yaml
	// publishes it on 127.0.0.1:3310).
	ClamAVAddr string `env:"FILES_CLAMAV_ADDR" envDefault:"127.0.0.1:3310"`
	// ClamAVTimeout bounds one scan: connecting, streaming the object and
	// waiting for the verdict. A timeout is "no verdict": the file stays
	// pending and is scanned again later (fail closed).
	ClamAVTimeout time.Duration `env:"FILES_CLAMAV_TIMEOUT" envDefault:"60s"`
	// ClamAVMaxConns is the size of the connection pool: at most this many
	// scans run at once per process; idle clamd sessions are reused.
	ClamAVMaxConns int `env:"FILES_CLAMAV_MAX_CONNS" envDefault:"4"`
	// ClamAVMaxBytes is the largest object streamed to clamd; it must equal
	// clamd's StreamMaxLength (25M in deploy/compose.dev.yaml) and cover
	// twice the largest browser upload (a rewritten file may grow, BE-059).
	ClamAVMaxBytes int64 `env:"FILES_CLAMAV_MAX_BYTES" envDefault:"26214400"`
	// ClamAVReadyCheck adds clamd (PING) to /readyz. Off by default: an
	// unavailable scanner only delays scans, it does not break the API.
	ClamAVReadyCheck bool `env:"FILES_CLAMAV_READY_CHECK" envDefault:"false"`
	// RescanBatch is how many skipped files files.rescan_skipped scans per
	// run once a real scanner is wired (the migration of files uploaded
	// with the no-op scanner).
	RescanBatch int `env:"FILES_RESCAN_BATCH" envDefault:"100"`
}

// Scanner names (FILES_SCANNER).
const (
	ScannerNoop   = ProviderNoop
	ScannerClamAV = "clamav"
)

// Scanners lists the accepted FILES_SCANNER values; cmd/koro has one
// constructor per name (wire_files.go).
func Scanners() []string { return []string{ScannerNoop, ScannerClamAV} }

// minClamAVBytes is twice the largest browser upload (02 §3.1: 10 MB): a
// skipped file is rescanned after its metadata removal, which may grow it
// up to that bound (internal/files maxGrowth). internal/files checks the
// same bound against the scanner at start.
const minClamAVBytes = 2 * 10 << 20

func (f *Files) validate(c Common) error {
	var errs []error
	if f.UploadURLTTL < time.Minute || f.UploadURLTTL > 10*time.Minute {
		errs = append(errs, errors.New("FILES_UPLOAD_URL_TTL must be between 1m and 10m (04 §1.14)"))
	}
	if err := checkProvider(c, "FILES_SCANNER", &f.Scanner, Scanners()); err != nil {
		errs = append(errs, err)
	} else if f.Scanner == ScannerClamAV {
		errs = append(errs, f.validateClamAV()...)
	}
	if f.RescanBatch < 1 || f.RescanBatch > 1000 {
		errs = append(errs, errors.New("FILES_RESCAN_BATCH must be between 1 and 1000 (01 §7: ≤ 1000 rows per batch)"))
	}
	// The no-op scanner is refused by checkProvider (a development adapter).
	if c.ProdLike() && !f.ScanRequired {
		errs = append(errs, fmt.Errorf("FILES_SCAN_REQUIRED=true is required in %s (real uploads are scanned, T-81)", c.Env))
	}
	return errors.Join(errs...)
}

func (f *Files) validateClamAV() []error {
	var errs []error
	f.ClamAVAddr = strings.TrimSpace(f.ClamAVAddr)
	if host, port, err := net.SplitHostPort(f.ClamAVAddr); err != nil || host == "" || !validPort(port) {
		errs = append(errs, errors.New("FILES_CLAMAV_ADDR must be host:port (clamd TCPSocket)"))
	}
	if f.ClamAVTimeout < time.Second || f.ClamAVTimeout > 10*time.Minute {
		errs = append(errs, errors.New("FILES_CLAMAV_TIMEOUT must be between 1s and 10m"))
	}
	if f.ClamAVMaxConns < 1 || f.ClamAVMaxConns > 64 {
		errs = append(errs, errors.New("FILES_CLAMAV_MAX_CONNS must be between 1 and 64"))
	}
	if f.ClamAVMaxBytes < minClamAVBytes || f.ClamAVMaxBytes > 4<<30-1 {
		errs = append(errs, fmt.Errorf("FILES_CLAMAV_MAX_BYTES must be at least %d (twice the largest upload) and below 4 GiB", minClamAVBytes))
	}
	return errs
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n < 1<<16
}
