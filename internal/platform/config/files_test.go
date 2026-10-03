package config_test

import (
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

type filesOnly struct {
	config.Common
	Files config.Files
}

func TestFiles_ScannerDefaultsAndValidation(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[filesOnly](storageEnv())
	if err != nil {
		t.Fatal(err)
	}
	f := cfg.Files
	if f.Scanner != config.ScannerNoop || f.ScanRequired || f.ClamAVAddr != "127.0.0.1:3310" || f.ClamAVTimeout != time.Minute ||
		f.ClamAVMaxConns != 4 || f.ClamAVMaxBytes != 25<<20 || f.ClamAVReadyCheck || f.RescanBatch != 100 {
		t.Fatalf("files defaults = %+v", f)
	}

	clam := func(kv ...string) map[string]string {
		return storageEnv(append([]string{"FILES_SCANNER", "clamav"}, kv...)...)
	}
	prod := func(kv ...string) map[string]string {
		return clam(append([]string{"APP_ENV", "prod", "FILES_SCAN_REQUIRED", "true", "FILES_CLAMAV_ADDR", "clamav:3310"}, kv...)...)
	}
	tests := []struct {
		name    string
		environ map[string]string
		wantErr string
	}{
		{"clamav with defaults", clam(), ""},
		{"clamav upper case, required, ready check", clam("FILES_SCANNER", " ClamAV ", "FILES_SCAN_REQUIRED", "true", "FILES_CLAMAV_READY_CHECK", "true"), ""},
		{"prod with clamav and scan required", prod(), ""},
		{"unknown scanner", storageEnv("FILES_SCANNER", "virustotal"), "FILES_SCANNER must be noop or clamav"},
		{"address without port", clam("FILES_CLAMAV_ADDR", "clamav"), "FILES_CLAMAV_ADDR"},
		{"address with bad port", clam("FILES_CLAMAV_ADDR", "clamav:99999"), "FILES_CLAMAV_ADDR"},
		{"timeout too short", clam("FILES_CLAMAV_TIMEOUT", "100ms"), "FILES_CLAMAV_TIMEOUT"},
		{"no connections", clam("FILES_CLAMAV_MAX_CONNS", "0"), "FILES_CLAMAV_MAX_CONNS"},
		{"stream limit below twice the upload limit", clam("FILES_CLAMAV_MAX_BYTES", "10485760"), "FILES_CLAMAV_MAX_BYTES"},
		{"rescan batch too large", storageEnv("FILES_RESCAN_BATCH", "5000"), "FILES_RESCAN_BATCH"},
		{"prod with the no-op scanner", prod("FILES_SCANNER", "noop"), "FILES_SCANNER=clamav and FILES_SCAN_REQUIRED=true are required in prod"},
		{"staging without scan required", prod("APP_ENV", "staging", "FILES_SCAN_REQUIRED", "false"), "required in staging"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load[filesOnly](tt.environ)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
