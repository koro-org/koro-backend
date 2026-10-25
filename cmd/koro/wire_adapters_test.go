package main

import (
	"slices"
	"strings"
	"testing"

	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/notify/pushtest"
	"koro/internal/platform/config"
	"koro/internal/platform/storage"
)

// TestWire_EveryConfiguredAdapterIsWired: each selection map has exactly one
// constructor per name its configuration key accepts (BE-113), so a name
// added on one side only fails here and not at a deployment.
func TestWire_EveryConfiguredAdapterIsWired(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	sorted := func(names []string) []string { return slices.Sorted(slices.Values(names)) }
	for _, p := range []struct {
		key         string
		wired, want []string
	}{
		{"NOTIFY_EMAIL_PROVIDER", emailTransports(config.Notify{}).names(), config.EmailProviders()},
		{"NOTIFY_PUSH_PROVIDER", pushProviders(config.Notify{}, c).names(), config.PushProviders()},
		{"STORAGE_PROVIDER", objectStores(config.Storage{}, c).names(), config.StorageProviders()},
		{"FILES_SCANNER", scanners(config.Files{}, c).names(), config.Scanners()},
	} {
		if !slices.Equal(p.wired, sorted(p.want)) {
			t.Errorf("%s: wired adapters %v, configuration accepts %v", p.key, p.wired, p.want)
		}
	}
}

// TestWire_AdapterSelectedByConfig: the configured name builds its adapter
// and the module is wired with it; an unknown name stops the build and
// names the key.
func TestWire_AdapterSelectedByConfig(t *testing.T) {
	t.Parallel()

	t.Run("in-memory e-mail", func(t *testing.T) {
		t.Parallel()
		c := newTestContainer(t)
		c.notify.transport = nil
		if err := c.build(); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.notify.deps.Transport.(*notify.MemTransport); !ok {
			t.Fatalf("transport = %T, want *notify.MemTransport", c.notify.deps.Transport)
		}
	})

	t.Run("in-memory storage", func(t *testing.T) {
		t.Parallel()
		c := newTestContainer(t)
		c.files.objects = nil
		c.infra.platform.Storage = config.Storage{Provider: config.StorageProviderMem,
			BucketFiles: testBuckets.Files, BucketQuarantine: testBuckets.Quarantine}
		if err := c.build(); err != nil {
			t.Fatal(err)
		}
		o, err := objectStores(c.infra.platform.Storage, c).pick("STORAGE_PROVIDER", config.StorageProviderMem)
		if _, ok := o.(*storage.Mem); err != nil || !ok {
			t.Fatalf("mem storage = %T, %v", o, err)
		}
		s3, err := objectStores(config.Storage{Endpoint: "http://127.0.0.1:8333", Region: "us-east-1",
			AccessKey: config.NewSecret("test-access"), SecretKey: config.NewSecret("test-secret")}, c).pick("STORAGE_PROVIDER", "s3")
		if _, ok := s3.(*storage.S3); err != nil || !ok {
			t.Fatalf("s3 storage = %T, %v", s3, err)
		}
	})

	t.Run("no-op scanner", func(t *testing.T) {
		t.Parallel()
		c := newTestContainer(t)
		sc, err := scanners(config.Files{}, c).pick("FILES_SCANNER", config.ScannerNoop)
		if _, ok := sc.(files.NoopScanner); err != nil || !ok {
			t.Fatalf("scanner = %T, %v", sc, err)
		}
	})

	t.Run("push providers listed explicitly", func(t *testing.T) {
		t.Parallel()
		fcm, apns := pushtest.NewFCM(t), pushtest.NewAPNs(t)
		c := newTestContainer(t)
		cfg := *c.notify.cfg
		cfg.PushFCMProjectID, cfg.PushFCMCredentials = fcm.ProjectID, config.NewSecret(string(fcm.Credentials))
		cfg.PushAPNsKey = config.NewSecret(string(apns.KeyPEM))
		cfg.PushAPNsKeyID, cfg.PushAPNsTeamID, cfg.PushAPNsTopic = apns.KeyID, apns.TeamID, apns.Topic

		cfg.PushProvider = config.PushProviderNoop
		if p, err := newPushSender(cfg, notify.NewMemStore(), c); err != nil || p.Name() != "noop" {
			t.Fatalf("noop although configured = %T, %v", p, err)
		}
		cfg.PushProvider = config.PushProviderAPNs
		p, err := newPushSender(cfg, notify.NewMemStore(), c)
		if _, ok := p.(*notify.PushTransport); err != nil || !ok {
			t.Fatalf("apns = %T, %v", p, err)
		}
	})

	t.Run("unknown adapter", func(t *testing.T) {
		t.Parallel()
		c := newTestContainer(t)
		c.notify.transport = nil
		c.notify.cfg.EmailProvider = "ses"
		err := c.build()
		if err == nil || !strings.Contains(err.Error(), `NOTIFY_EMAIL_PROVIDER="ses": no such adapter (wired: mem, smtp)`) {
			t.Fatalf("build() error = %v", err)
		}
	})
}
