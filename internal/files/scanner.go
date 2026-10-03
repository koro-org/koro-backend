package files

import (
	"context"
	"io"
	"uuid"
)

// Scanner checks an uploaded object for malware (05 §14.7 "ClamAV"). The
// files module calls it from the file.uploaded worker handler, after the
// size, checksum and magic bytes were verified and before the object leaves
// the quarantine bucket.
//
// The contract is written so that the ClamAV adapter of BE-048 plugs in
// without changes to the flow:
//
//   - Scan reads the content only through obj.Open, lazily: a scanner that
//     does not look at the content (NoopScanner) never downloads it.
//   - A verdict (clean, infected, skipped) is returned with a nil error. Any
//     error means "no verdict" (timeout, scanner down, read failure): the
//     file stays pending and the task is retried; files.rescan_pending
//     re-enqueues it if the retries are exhausted. A scanner never turns an
//     error into clean.
//   - Scan must be idempotent and safe to call concurrently.
//   - Signature names a finding for logs and audit; it never contains file
//     content or personal data.
type Scanner interface {
	// Name identifies the scanner in logs ("noop", "clamav").
	Name() string
	// Real reports whether the scanner inspects content. FILES_SCAN_REQUIRED
	// needs a real scanner.
	Real() bool
	// Scan returns the verdict on obj.
	Scan(ctx context.Context, obj ScanObject) (ScanVerdict, error)
}

// ScanObject is what a scanner may inspect.
type ScanObject struct {
	FileID  uuid.UUID
	Purpose Purpose
	MIME    string // verified by magic bytes
	Size    int64  // verified
	// Open streams the object (at most Size bytes). The scanner closes it.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

// ScanVerdict is the result of one scan.
type ScanVerdict struct {
	Status    ScanStatus // ScanClean, ScanInfected or ScanSkipped
	Signature string     // infected only
}

// NoopScanner is the scanner until ClamAV (BE-048): it never reads the
// object and marks every file skipped (02 §3.1 scan_status='skipped').
// With FILES_SCAN_REQUIRED=false a skipped file is downloadable; with true
// it is not, so the wiring refuses that combination.
type NoopScanner struct{}

// Name implements Scanner.
func (NoopScanner) Name() string { return "noop" }

// Real implements Scanner.
func (NoopScanner) Real() bool { return false }

// Scan implements Scanner.
func (NoopScanner) Scan(context.Context, ScanObject) (ScanVerdict, error) {
	return ScanVerdict{Status: ScanSkipped}, nil
}
