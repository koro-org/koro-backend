package config_test

import (
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

// TestConfig_BE006Sections: defaults and validation of the queue, outbox,
// idempotency, inbox and ratelimit sections (BE-006).
func TestConfig_BE006Sections(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadAPI(validAPIEnv())
	if err != nil {
		t.Fatalf("LoadAPI() error = %v", err)
	}
	q, o, p := cfg.Platform.Queue, cfg.Platform.Outbox, cfg.Platform
	if q.RedisURL.Reveal() != "redis://localhost:6379/0" || q.RedisPoolSize != 30 || q.WorkerConcurrency != 10 ||
		!q.SchedulerEnabled || q.ShutdownTimeout != 25*time.Second || q.HealthAddr != "127.0.0.1:8081" {
		t.Fatalf("queue defaults: %+v", q)
	}
	if o.RelayBatch != 100 || o.RelayIdle != time.Second || o.RelayBackoffMax != 10*time.Minute || o.Retention != 7*24*time.Hour {
		t.Fatalf("outbox defaults: %+v", o)
	}
	if p.Idempotency.TTL != 24*time.Hour || p.Inbox.PayloadRetention != 90*24*time.Hour || p.Inbox.RowRetention != 730*24*time.Hour {
		t.Fatalf("idempotency/inbox defaults: %+v %+v", p.Idempotency, p.Inbox)
	}
	if !cfg.RateLimit.Enabled {
		t.Fatal("rate limiting is off by default")
	}
	if _, err := config.LoadAPI(validAPIEnv("WORKER_HEALTH_ADDR", "")); err != nil {
		t.Fatalf("empty WORKER_HEALTH_ADDR (off): %v", err)
	}

	bad := []struct {
		name    string
		environ map[string]string
		want    string
	}{
		{"missing redis", validAPIEnv("REDIS_URL", ""), `"REDIS_URL" should not be empty`},
		{"http redis", validAPIEnv("REDIS_URL", "http://:s3cr3t-pw@redis:6379"), "REDIS_URL must be a redis://"},
		{"pool", validAPIEnv("REDIS_POOL_SIZE", "0"), "REDIS_POOL_SIZE"},
		{"concurrency", validAPIEnv("WORKER_CONCURRENCY", "0"), "WORKER_CONCURRENCY"},
		{"health addr", validAPIEnv("WORKER_HEALTH_ADDR", "8081"), "WORKER_HEALTH_ADDR"},
		{"batch", validAPIEnv("OUTBOX_RELAY_BATCH", "5000"), "OUTBOX_RELAY_BATCH"},
		{"retention", validAPIEnv("OUTBOX_RETENTION", "1m"), "OUTBOX_RETENTION"},
		{"ttl", validAPIEnv("IDEMPOTENCY_TTL", "10m"), "IDEMPOTENCY_TTL"},
		{"webhook rows shorter than payloads", validAPIEnv("WEBHOOK_ROW_RETENTION", "24h"), "WEBHOOK_ROW_RETENTION"},
		{"rate limit off in prod", validAPIEnv("APP_ENV", "prod", "RATE_LIMIT_ENABLED", "false",
			"PUBLIC_BASE_URL", "https://koro.example", "TRUSTED_ORIGINS", "https://koro.example"), "RATE_LIMIT_ENABLED"},
	}
	for _, b := range bad {
		_, err := config.LoadAPI(b.environ)
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s: error = %v, want %q", b.name, err, b.want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t-pw") {
			t.Errorf("%s: error leaks the Redis password", b.name)
		}
	}

	// The worker needs Redis too.
	w := validAPIEnv()
	delete(w, "REDIS_URL")
	if _, err := config.LoadWorker(w); err == nil || !strings.Contains(err.Error(), "REDIS_URL") {
		t.Fatalf("LoadWorker() without REDIS_URL = %v", err)
	}
}
