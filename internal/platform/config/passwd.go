package config

import (
	"errors"
	"fmt"

	"koro/internal/platform/passwd"
)

// Passwd configures password hashing (internal/platform/passwd; 05 §14.1).
// API and seed only. Owner: BE-003.
type Passwd struct {
	// Pepper is mixed into every password with HMAC before argon2id. It is
	// not stored in the database; staging/prod read it from a Docker secret
	// (PASSWORD_PEPPER_FILE). Changing it invalidates every password hash.
	Pepper Secret `env:"PASSWORD_PEPPER,required,notEmpty"`
	// Concurrency bounds simultaneous argon2id runs (19 MiB each).
	Concurrency int `env:"PASSWORD_HASH_CONCURRENCY" envDefault:"4"`
}

func (p *Passwd) validate(c Common) error {
	var errs []error
	switch {
	case len(p.Pepper.Reveal()) < passwd.MinPepperLen:
		errs = append(errs, fmt.Errorf("PASSWORD_PEPPER must be at least %d bytes", passwd.MinPepperLen))
	case c.ProdLike() && weakSecretText(p.Pepper.Reveal()):
		errs = append(errs, errors.New("PASSWORD_PEPPER looks like a placeholder in staging/prod (generate one: openssl rand -base64 48)"))
	}
	if p.Concurrency < 1 || p.Concurrency > 64 {
		errs = append(errs, errors.New("PASSWORD_HASH_CONCURRENCY must be between 1 and 64"))
	}
	return errors.Join(errs...)
}
