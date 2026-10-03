package config

import (
	"errors"
	"time"
)

// Files configures the files module (internal/files). Owner: BE-008; only
// tickets of the files module edit this file and the "# --- files ---" block
// of .env.example (BE-001 R7).
type Files struct {
	// ScanRequired makes downloads require scan_status=clean. While false
	// (P0–P2, until ClamAV arrives with BE-048) a file whose scan was
	// skipped is downloadable too. true needs a real scanner: the wiring
	// refuses to start with the no-op scanner.
	ScanRequired bool `env:"FILES_SCAN_REQUIRED" envDefault:"false"`
	// UploadURLTTL is the lifetime of a presigned upload URL (04 §1.14:
	// 10 minutes; at most storage.MaxPutTTL).
	UploadURLTTL time.Duration `env:"FILES_UPLOAD_URL_TTL" envDefault:"10m"`
}

func (f *Files) validate(Common) error {
	if f.UploadURLTTL < time.Minute || f.UploadURLTTL > 10*time.Minute {
		return errors.New("FILES_UPLOAD_URL_TTL must be between 1m and 10m (04 §1.14)")
	}
	return nil
}
