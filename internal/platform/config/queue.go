package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"
)

// Queue configures Redis and the asynq worker/scheduler
// (internal/platform/queue; 05 §8.5). Owner: BE-006. Used by `koro api`
// (enqueue, rate limit) and `koro worker`.
type Queue struct {
	// RedisURL is redis://[:password@]host:port/db or rediss://... It may
	// carry a password: prod gives it as REDIS_URL_FILE. Redis must run
	// with AOF and maxmemory-policy noeviction (R-002 §5.3).
	RedisURL      Secret `env:"REDIS_URL,required,notEmpty"`
	RedisPoolSize int    `env:"REDIS_POOL_SIZE" envDefault:"30"`
	// WorkerConcurrency is the number of tasks one worker runs in parallel.
	WorkerConcurrency int `env:"WORKER_CONCURRENCY" envDefault:"10"`
	// SchedulerEnabled runs the cron scheduler in this worker. Every job is
	// Unique, so several schedulers are safe; switching it off on all but one
	// replica saves Redis round trips (05 §8.5).
	SchedulerEnabled bool `env:"WORKER_SCHEDULER_ENABLED" envDefault:"true"`
	// ShutdownTimeout is how long running tasks may finish on SIGTERM; the
	// rest go back to the queue (stop_grace_period must exceed it).
	ShutdownTimeout time.Duration `env:"WORKER_SHUTDOWN_TIMEOUT" envDefault:"25s"`
	// HealthAddr serves the worker's /healthz and /readyz (06 H-26; empty =
	// off). Loopback by default: the Docker HEALTHCHECK runs inside the
	// container (`koro healthcheck -url http://127.0.0.1:8081/healthz`).
	HealthAddr string `env:"WORKER_HEALTH_ADDR" envDefault:"127.0.0.1:8081"`
}

func (q *Queue) validate(Common) error {
	var errs []error
	u, err := url.Parse(q.RedisURL.Reveal())
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Host == "" {
		// Never echo the value: it may carry a password.
		errs = append(errs, errors.New("REDIS_URL must be a redis://host:port/db or rediss:// URL"))
	}
	if q.RedisPoolSize < 1 || q.RedisPoolSize > 1000 {
		errs = append(errs, fmt.Errorf("REDIS_POOL_SIZE must be between 1 and 1000 (got %d)", q.RedisPoolSize))
	}
	if q.WorkerConcurrency < 1 || q.WorkerConcurrency > 200 {
		errs = append(errs, fmt.Errorf("WORKER_CONCURRENCY must be between 1 and 200 (got %d)", q.WorkerConcurrency))
	}
	if q.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("WORKER_SHUTDOWN_TIMEOUT must be positive"))
	}
	if q.HealthAddr != "" {
		if _, _, err := net.SplitHostPort(q.HealthAddr); err != nil {
			errs = append(errs, fmt.Errorf("WORKER_HEALTH_ADDR must be host:port or empty: %w", err))
		}
	}
	return errors.Join(errs...)
}
