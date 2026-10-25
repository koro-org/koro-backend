package config_test

import (
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

type storageOnly struct {
	config.Common
	Storage config.Storage
	Files   config.Files
}

func storageEnv(kv ...string) map[string]string {
	m := map[string]string{"APP_ENV": "dev"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestStorage_Defaults_TargetLocalSeaweedFS(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[storageOnly](storageEnv())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	s, f := cfg.Storage, cfg.Files
	if s.Endpoint != "http://127.0.0.1:8333" || s.Region != "us-east-1" || !s.PathStyle || s.Timeout != 30*time.Second ||
		s.BucketFiles != "koro-files-dev" || s.BucketQuarantine != "koro-quarantine-dev" ||
		s.AccessKey.Reveal() != "koro-dev-access-key" || s.SecretKey.Reveal() != "dev-only-change-me" {
		t.Fatalf("storage defaults = %+v, want deploy/compose.dev.yaml SeaweedFS", s)
	}
	if f.ScanRequired || f.UploadURLTTL != 10*time.Minute {
		t.Fatalf("files defaults = %+v", f)
	}
	if s.PublicMedia() || s.BucketPublic != "" || s.PublicBaseURL != "" {
		t.Fatalf("public media is on by default: %+v", s)
	}
}

func TestStorage_PublicBaseURLLosesTrailingSlash(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[storageOnly](storageEnv("S3_BUCKET_PUBLIC", "koro-public-dev",
		"S3_PUBLIC_BASE_URL", " http://127.0.0.1:8333/koro-public-dev/ "))
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Storage; !s.PublicMedia() || s.PublicBaseURL != "http://127.0.0.1:8333/koro-public-dev" {
		t.Fatalf("storage = %+v", s)
	}
}

func TestStorage_Validation(t *testing.T) {
	t.Parallel()
	prod := func(kv ...string) map[string]string {
		m := storageEnv("APP_ENV", "prod", "S3_ENDPOINT", "https://s3.tr-1.provider.example", "S3_ACCESS_KEY", "AKPROD",
			"S3_SECRET_KEY", "prod-secret-value", "S3_BUCKET_FILES", "koro-files", "S3_BUCKET_QUARANTINE", "koro-quarantine",
			"FILES_SCANNER", "clamav", "FILES_SCAN_REQUIRED", "true", "FILES_CLAMAV_ADDR", "clamav:3310")
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name    string
		environ map[string]string
		wantErr string
	}{
		{"dev defaults", storageEnv(), ""},
		{"prod with real settings", prod(), ""},
		{"prod with a public presign host", prod("S3_PRESIGN_ENDPOINT", "https://files.koro.example"), ""},
		{"staging rejects every dev default", storageEnv("APP_ENV", "staging"), "must use https"},
		{"prod rejects dev credentials", prod("S3_SECRET_KEY", "dev-only-change-me"), "development placeholders"},
		{"prod rejects dev buckets", prod("S3_BUCKET_FILES", "koro-files-dev"), "development buckets"},
		{"prod rejects loopback", prod("S3_ENDPOINT", "https://127.0.0.1:8333"), "loopback"},
		{"endpoint with credentials", storageEnv("S3_ENDPOINT", "http://user:pw@127.0.0.1:8333"), "without credentials"},
		{"not a url", storageEnv("S3_ENDPOINT", "127.0.0.1:8333"), "S3_ENDPOINT must be an http(s) URL"},
		{"same buckets", storageEnv("S3_BUCKET_QUARANTINE", "koro-files-dev"), "must differ"},
		{"bad bucket name", storageEnv("S3_BUCKET_FILES", "Koro_Files"), "valid S3 bucket name"},
		{"empty secret", storageEnv("S3_SECRET_KEY", " "), "S3_SECRET_KEY must be set"},
		{"timeout too long", storageEnv("S3_TIMEOUT", "10m"), "S3_TIMEOUT"},
		{"upload url ttl over 10m", storageEnv("FILES_UPLOAD_URL_TTL", "11m"), "FILES_UPLOAD_URL_TTL"},
		{"upload url ttl too short", storageEnv("FILES_UPLOAD_URL_TTL", "10s"), "FILES_UPLOAD_URL_TTL"},
		{"prod with public media", prod("S3_BUCKET_PUBLIC", "koro-public", "S3_PUBLIC_BASE_URL", "https://media.koro.example/"), ""},
		{"public bucket without base url", storageEnv("S3_BUCKET_PUBLIC", "koro-public-dev"), "set together"},
		{"public base url without bucket", storageEnv("S3_PUBLIC_BASE_URL", "http://127.0.0.1:8333/koro-public-dev"), "set together"},
		{"public bucket is the files bucket", storageEnv("S3_BUCKET_PUBLIC", "koro-files-dev", "S3_PUBLIC_BASE_URL", "http://127.0.0.1:8333/x"), "must differ"},
		{"public base url with a query", storageEnv("S3_BUCKET_PUBLIC", "koro-public-dev", "S3_PUBLIC_BASE_URL", "http://127.0.0.1:8333/x?a=1"), "without credentials, query"},
		{"prod public base url over http", prod("S3_BUCKET_PUBLIC", "koro-public", "S3_PUBLIC_BASE_URL", "http://media.koro.example"), "S3_PUBLIC_BASE_URL must use https"},
		{"prod public dev bucket", prod("S3_BUCKET_PUBLIC", "koro-public-dev", "S3_PUBLIC_BASE_URL", "https://media.koro.example"), "development bucket"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load[storageOnly](tt.environ)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "prod-secret-value") {
				t.Fatal("the error leaks the secret key")
			}
		})
	}
}
