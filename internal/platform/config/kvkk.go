package config

import (
	"errors"
	"fmt"
	"net/mail"
	"time"
)

// MaxExportRetention is the longest a data export may live (T-83: export
// files are deleted after 7 days; the deletion is never deferred).
const MaxExportRetention = 7 * 24 * time.Hour

// Kvkk configures the kvkk module (internal/kvkk). Owner: BE-021; only
// tickets of the kvkk module edit this file and the "# --- kvkk ---" block
// of .env.example (BE-001 R7).
type Kvkk struct {
	// DPOEmail is the mailbox of the data protection officer (veri sorumlusu
	// irtibat kişisi): new requests and the 7-day deadline digest go there.
	// The messages carry request IDs, types and due dates only. Mandatory in
	// staging and prod; empty in dev logs a warning instead of mailing.
	DPOEmail string `env:"KVKK_DPO_EMAIL"`
	// ExportRetention is how long a data export file is kept before the
	// hourly kvkk.purge_exports job deletes it (1h–168h).
	ExportRetention time.Duration `env:"KVKK_EXPORT_RETENTION" envDefault:"168h"`
}

func (s *Kvkk) validate(c Common) error {
	var errs []error
	switch {
	case s.DPOEmail == "" && c.ProdLike():
		errs = append(errs, errors.New("KVKK_DPO_EMAIL is required in staging/prod"))
	case s.DPOEmail != "":
		if a, err := mail.ParseAddress(s.DPOEmail); err != nil || a.Address != s.DPOEmail {
			errs = append(errs, errors.New("KVKK_DPO_EMAIL must be a bare e-mail address"))
		}
	}
	if s.ExportRetention < time.Hour || s.ExportRetention > MaxExportRetention {
		errs = append(errs, fmt.Errorf("KVKK_EXPORT_RETENTION must be between 1h and %s (T-83)", MaxExportRetention))
	}
	return errors.Join(errs...)
}
