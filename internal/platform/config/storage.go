package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Storage configures S3-compatible object storage (internal/platform/storage;
// 05 §14.7: S3_ENDPOINT, S3_BUCKET_*, S3_ACCESS_KEY, S3_SECRET_KEY). Owner:
// BE-008, BE-113 (STORAGE_PROVIDER), BE-120 (public media) (BE-001 R7: only
// the owning ticket edits this file).
//
// The defaults point at the local SeaweedFS of deploy/compose.dev.yaml (its
// credentials are in deploy/seaweedfs/s3.dev.json). They are rejected in
// staging and prod (see validate), so a forgotten variable stops the process
// at start instead of failing every upload later (05 §12.2). Both buckets
// must be private; the quarantine bucket needs a 1-day lifecycle rule
// (05 §14.7), configured by operations on the provider.
type Storage struct {
	// Provider selects the storage.Objects adapter (STORAGE_PROVIDER,
	// BE-113): "s3" (any S3-compatible server) or "mem" (in memory,
	// development only; refused in staging and prod). The S3_* settings are
	// checked only for s3.
	Provider string `env:"STORAGE_PROVIDER" envDefault:"s3"`
	// Endpoint is the S3 API the api/worker processes talk to.
	Endpoint string `env:"S3_ENDPOINT" envDefault:"http://127.0.0.1:8333"`
	// PresignEndpoint is the S3 API host browsers reach; presigned URLs
	// are signed for it. Empty means S3_ENDPOINT.
	PresignEndpoint string        `env:"S3_PRESIGN_ENDPOINT"`
	Region          string        `env:"S3_REGION" envDefault:"us-east-1"`
	AccessKey       Secret        `env:"S3_ACCESS_KEY" envDefault:"koro-dev-access-key"`
	SecretKey       Secret        `env:"S3_SECRET_KEY" envDefault:"dev-only-change-me"`
	PathStyle       bool          `env:"S3_PATH_STYLE" envDefault:"true"`
	Timeout         time.Duration `env:"S3_TIMEOUT" envDefault:"30s"`
	// BucketFiles holds verified (and scanned) objects; BucketQuarantine
	// receives every browser upload until files moves it (05 §14.7).
	BucketFiles      string `env:"S3_BUCKET_FILES" envDefault:"koro-files-dev"`
	BucketQuarantine string `env:"S3_BUCKET_QUARANTINE" envDefault:"koro-quarantine-dev"`
	// BucketPublic is the public media bucket (BE-120): files copies the
	// cleared org logos and event covers into it under content-addressed
	// keys; operations make it anonymously readable without listing. Empty
	// (with PublicBaseURL empty) turns public media off: the APIs return
	// only the short-lived signed URLs.
	BucketPublic string `env:"S3_BUCKET_PUBLIC"`
	// PublicBaseURL is the origin (and optional path) the public bucket's
	// objects are reached under by browsers and crawlers, e.g.
	// "https://media.koro.example" or "https://s3.example/koro-public". An
	// object's URL is PublicBaseURL + "/" + key.
	PublicBaseURL string `env:"S3_PUBLIC_BASE_URL"`
}

// PublicMedia reports whether the public media bucket is configured.
func (s Storage) PublicMedia() bool { return s.BucketPublic != "" && s.PublicBaseURL != "" }

// Storage adapters (STORAGE_PROVIDER).
const (
	StorageProviderS3  = "s3"
	StorageProviderMem = ProviderMem
)

// StorageProviders lists the accepted STORAGE_PROVIDER values; cmd/koro has
// one constructor per name (wire_files.go).
func StorageProviders() []string { return []string{StorageProviderS3, StorageProviderMem} }

// Development placeholders of .env.example / s3.dev.json.
const (
	devS3AccessKey = "koro-dev-access-key"
	devS3SecretKey = "dev-only-change-me" //nolint:gosec // G101: dev-only placeholder, rejected in staging/prod
)

// bucketRe is the S3 bucket naming rule (lower case, digits, dots, hyphens).
var bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func (s *Storage) validate(c Common) error {
	if err := checkProvider(c, "STORAGE_PROVIDER", &s.Provider, StorageProviders()); err != nil {
		return err
	}
	var errs []error
	if s.Provider == StorageProviderS3 {
		errs = s.validateS3(c)
	}
	// The bucket names belong to every adapter (files keeps uploads apart).
	for name, b := range map[string]string{"S3_BUCKET_FILES": s.BucketFiles, "S3_BUCKET_QUARANTINE": s.BucketQuarantine} {
		if !bucketRe.MatchString(b) {
			errs = append(errs, fmt.Errorf("%s must be a valid S3 bucket name", name))
		}
	}
	if s.BucketFiles == s.BucketQuarantine {
		errs = append(errs, errors.New("S3_BUCKET_FILES and S3_BUCKET_QUARANTINE must differ (quarantine, 05 §14.7)"))
	}
	if c.ProdLike() && (strings.HasSuffix(s.BucketFiles, "-dev") || strings.HasSuffix(s.BucketQuarantine, "-dev")) {
		errs = append(errs, fmt.Errorf("S3_BUCKET_* are the development buckets, which is not allowed in %s", c.Env))
	}
	errs = append(errs, s.validatePublic(c)...)
	return errors.Join(errs...)
}

// validatePublic checks the public media settings (BE-120): both or none;
// a bucket of its own; an http(s) base URL without credentials, query or
// fragment (https and not loopback in staging/prod).
func (s *Storage) validatePublic(c Common) []error {
	s.BucketPublic = strings.TrimSpace(s.BucketPublic)
	s.PublicBaseURL = strings.TrimRight(strings.TrimSpace(s.PublicBaseURL), "/")
	switch {
	case s.BucketPublic == "" && s.PublicBaseURL == "":
		return nil
	case s.BucketPublic == "" || s.PublicBaseURL == "":
		return []error{errors.New("S3_BUCKET_PUBLIC and S3_PUBLIC_BASE_URL are set together (public media) or not at all")}
	}
	var errs []error
	switch {
	case !bucketRe.MatchString(s.BucketPublic):
		errs = append(errs, errors.New("S3_BUCKET_PUBLIC must be a valid S3 bucket name"))
	case s.BucketPublic == s.BucketFiles || s.BucketPublic == s.BucketQuarantine:
		errs = append(errs, errors.New("S3_BUCKET_PUBLIC must differ from S3_BUCKET_FILES and S3_BUCKET_QUARANTINE (only cleared images are public)"))
	case c.ProdLike() && strings.HasSuffix(s.BucketPublic, "-dev"):
		errs = append(errs, fmt.Errorf("S3_BUCKET_PUBLIC is a development bucket, which is not allowed in %s", c.Env))
	}
	u, err := url.Parse(s.PublicBaseURL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		errs = append(errs, errors.New("S3_PUBLIC_BASE_URL must be an http(s) URL without credentials, query or fragment"))
	case c.ProdLike() && u.Scheme != "https":
		errs = append(errs, fmt.Errorf("S3_PUBLIC_BASE_URL must use https in %s", c.Env))
	case c.ProdLike() && isLoopbackHost(u.Hostname()):
		errs = append(errs, fmt.Errorf("S3_PUBLIC_BASE_URL must not be a loopback address in %s", c.Env))
	}
	return errs
}

// validateS3 checks the connection settings of the s3 adapter.
func (s *Storage) validateS3(c Common) []error {
	var errs []error
	endpoint := func(name, v string) {
		u, err := url.Parse(v)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil:
			errs = append(errs, fmt.Errorf("%s must be an http(s) URL without credentials", name))
		case c.ProdLike() && u.Scheme != "https":
			errs = append(errs, fmt.Errorf("%s must use https in %s", name, c.Env))
		case c.ProdLike() && isLoopbackHost(u.Hostname()):
			errs = append(errs, fmt.Errorf("%s must not be a loopback address in %s", name, c.Env))
		}
	}
	s.Endpoint = strings.TrimSpace(s.Endpoint)
	endpoint("S3_ENDPOINT", s.Endpoint)
	if s.PresignEndpoint = strings.TrimSpace(s.PresignEndpoint); s.PresignEndpoint != "" {
		endpoint("S3_PRESIGN_ENDPOINT", s.PresignEndpoint)
	}
	if s.Region = strings.TrimSpace(s.Region); s.Region == "" {
		errs = append(errs, errors.New("S3_REGION must not be empty"))
	}
	if s.AccessKey.IsZero() || s.SecretKey.IsZero() {
		errs = append(errs, errors.New("S3_ACCESS_KEY and S3_SECRET_KEY must be set"))
	}
	if c.ProdLike() && (s.AccessKey.Reveal() == devS3AccessKey || s.SecretKey.Reveal() == devS3SecretKey) {
		errs = append(errs, fmt.Errorf("S3_ACCESS_KEY/S3_SECRET_KEY are development placeholders, which is not allowed in %s", c.Env))
	}
	if s.Timeout <= 0 || s.Timeout > 5*time.Minute {
		errs = append(errs, errors.New("S3_TIMEOUT must be positive and at most 5m"))
	}
	return errs
}
