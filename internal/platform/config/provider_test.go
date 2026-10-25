package config_test

import (
	"slices"
	"strings"
	"testing"

	"koro/internal/platform/config"
)

type adaptersOnly struct {
	config.Common
	Storage config.Storage
	Files   config.Files
	Notify  config.Notify
}

// adapterProdEnv is a complete prod configuration of the four ports.
func adapterProdEnv(kv ...string) map[string]string {
	m := map[string]string{
		"APP_ENV": "prod", "S3_ENDPOINT": "https://s3.tr-1.provider.example", "S3_ACCESS_KEY": "AKPROD",
		"S3_SECRET_KEY": "prod-secret-value", "S3_BUCKET_FILES": "koro-files", "S3_BUCKET_QUARANTINE": "koro-quarantine",
		"FILES_SCANNER": "clamav", "FILES_SCAN_REQUIRED": "true", "FILES_CLAMAV_ADDR": "clamav:3310",
		"SMTP_HOST": "smtp.koro.example.org", "SMTP_PORT": "587", "SMTP_TLS": "starttls", "SMTP_FROM": "noreply@bilet.koro.app",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// TestProviders_DefaultsKeepTodaysAdapters: without the new keys every port
// gets the adapter it had before BE-113 (SMTP, S3, no-op scanner in dev,
// push by the presence of settings).
func TestProviders_DefaultsKeepTodaysAdapters(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[adaptersOnly](storageEnv())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.EmailProvider != config.EmailProviderSMTP || cfg.Storage.Provider != config.StorageProviderS3 ||
		cfg.Files.Scanner != config.ScannerNoop || cfg.Notify.PushProvider != config.PushProviderAuto {
		t.Fatalf("defaults: email %q, storage %q, scanner %q, push %q", cfg.Notify.EmailProvider, cfg.Storage.Provider,
			cfg.Files.Scanner, cfg.Notify.PushProvider)
	}
	if got := cfg.Notify.ActivePushProviders(); len(got) != 0 {
		t.Fatalf("push without settings = %v, want the no-op adapter", got)
	}
	if _, err := config.Load[adaptersOnly](adapterProdEnv()); err != nil {
		t.Fatalf("prod defaults: %v", err)
	}
}

// TestProviders_DevAdaptersRefusedInProdLike: mem and noop adapters run in
// dev only; an unknown name stops the process with the accepted names.
func TestProviders_DevAdaptersRefusedInProdLike(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		environ map[string]string
		wantErr string
	}{
		{"dev in-memory e-mail", storageEnv("NOTIFY_EMAIL_PROVIDER", " MEM "), ""},
		{"dev in-memory storage", storageEnv("STORAGE_PROVIDER", "mem"), ""},
		{"dev no-op push", storageEnv("NOTIFY_PUSH_PROVIDER", "noop"), ""},
		{"prod in-memory e-mail", adapterProdEnv("NOTIFY_EMAIL_PROVIDER", "mem"), "NOTIFY_EMAIL_PROVIDER=mem is a development adapter, which is not allowed in prod"},
		{"staging in-memory storage", adapterProdEnv("APP_ENV", "staging", "STORAGE_PROVIDER", "mem"), "STORAGE_PROVIDER=mem is a development adapter, which is not allowed in staging"},
		{"prod no-op scanner", adapterProdEnv("FILES_SCANNER", "noop"), "FILES_SCANNER=noop is a development adapter"},
		{"prod no-op push", adapterProdEnv("NOTIFY_PUSH_PROVIDER", "noop"), "NOTIFY_PUSH_PROVIDER=noop is a development adapter"},
		{"unknown e-mail adapter", storageEnv("NOTIFY_EMAIL_PROVIDER", "ses"), "NOTIFY_EMAIL_PROVIDER must be smtp or mem"},
		{"unknown storage adapter", storageEnv("STORAGE_PROVIDER", "gcs"), "STORAGE_PROVIDER must be s3 or mem"},
		{"empty storage adapter", storageEnv("STORAGE_PROVIDER", " "), "STORAGE_PROVIDER must be s3 or mem"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load[adaptersOnly](tt.environ)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestProviders_AdapterSettingsCheckedOnlyForTheirAdapter: the SMTP and S3
// connection settings belong to their adapter; the sender identity and the
// bucket names belong to every adapter.
func TestProviders_AdapterSettingsCheckedOnlyForTheirAdapter(t *testing.T) {
	t.Parallel()
	ok := storageEnv("NOTIFY_EMAIL_PROVIDER", "mem", "SMTP_PORT", "0", "STORAGE_PROVIDER", "mem", "S3_ENDPOINT", "not a url")
	if _, err := config.Load[adaptersOnly](ok); err != nil {
		t.Fatalf("mem adapters with broken SMTP/S3 settings: %v", err)
	}
	for _, kv := range [][]string{
		{"NOTIFY_EMAIL_PROVIDER", "mem", "SMTP_FROM", "Koro <noreply@koro.test>"},
		{"STORAGE_PROVIDER", "mem", "S3_BUCKET_QUARANTINE", "koro-files-dev"},
		{"SMTP_PORT", "0"},
		{"S3_ENDPOINT", "not a url"},
	} {
		if _, err := config.Load[adaptersOnly](storageEnv(kv...)); err == nil {
			t.Errorf("Load(%v) succeeded", kv)
		}
	}
}

// TestProviders_PushSelection (NOTIFY_PUSH_PROVIDER): auto keeps the
// BE-055 behaviour, a list builds exactly the listed providers, each of
// which must be configured.
func TestProviders_PushSelection(t *testing.T) {
	t.Parallel()
	fcm := []string{"PUSH_FCM_PROJECT_ID", "koro-prod-123", "PUSH_FCM_CREDENTIALS", `{"type":"service_account"}`}
	apns := []string{"PUSH_APNS_KEY", "test-only-apns-key", "PUSH_APNS_KEY_ID", "ABC123DEFG",
		"PUSH_APNS_TEAM_ID", "TEAM123456", "PUSH_APNS_TOPIC", "app.koro.mobile"}
	both := append(append([]string{}, fcm...), apns...)
	with := func(provider string, kv []string) map[string]string {
		return storageEnv(append([]string{"NOTIFY_PUSH_PROVIDER", provider}, kv...)...)
	}
	tests := []struct {
		name    string
		environ map[string]string
		want    []string
		wantErr string
	}{
		{"auto, both configured", with("auto", both), []string{"fcm", "apns"}, ""},
		{"auto, fcm configured", with("auto", fcm), []string{"fcm"}, ""},
		{"fcm only although apns is configured", with("fcm", both), []string{"fcm"}, ""},
		{"list in any order and case", with(" APNS , fcm ", both), []string{"fcm", "apns"}, ""},
		{"noop although configured", with("noop", both), nil, ""},
		{"listed but not configured", with("apns", fcm), nil, "NOTIFY_PUSH_PROVIDER lists apns"},
		{"unknown provider", with("fcm,hms", both), nil, "NOTIFY_PUSH_PROVIDER must be auto, noop or a list of fcm or apns"},
		{"repeated provider", with("fcm,fcm", fcm), nil, "without repeats"},
		{"auto in a list", with("auto,fcm", fcm), nil, "NOTIFY_PUSH_PROVIDER must be"},
		{"required with noop", with("noop", append([]string{"PUSH_REQUIRED", "true"}, fcm...)), nil, "PUSH_REQUIRED=true needs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load[adaptersOnly](tt.environ)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			case err != nil && strings.Contains(err.Error(), "test-only-apns-key"):
				t.Fatalf("error leaks the key: %v", err)
			case err == nil:
				if got := cfg.Notify.ActivePushProviders(); !slices.Equal(got, tt.want) {
					t.Fatalf("ActivePushProviders() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
