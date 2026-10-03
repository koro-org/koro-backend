package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"koro/internal/platform/db"
)

// OpenRedis connects to REDIS_URL (redis:// or rediss://) and pings it.
// Errors never contain the URL (it may carry a password).
func OpenRedis(ctx context.Context, rawURL string, poolSize int) (*redis.Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") {
		return nil, errors.New("queue: REDIS_URL must be a redis:// or rediss:// URL")
	}
	opt, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("queue: invalid REDIS_URL")
	}
	if poolSize > 0 {
		opt.PoolSize = poolSize
	}
	opt.DialTimeout, opt.ReadTimeout, opt.WriteTimeout, opt.PoolTimeout = redisDialTimeout, redisRWTimeout, redisRWTimeout, redisPoolTimeout
	rdb := redis.NewClient(opt)
	if err := Ping(ctx, rdb); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return rdb, nil
}

// Ping checks that Redis answers (/readyz of api and worker).
func Ping(ctx context.Context, rdb redis.UniversalClient) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("queue: redis ping: %w", err)
	}
	return nil
}

// CheckPersistence verifies the Redis settings asynq needs (R-002 §5.3):
// AOF on and maxmemory-policy noeviction (an evicted task is lost silently).
// A Redis that forbids CONFIG GET (managed services) reports nothing.
func CheckPersistence(ctx context.Context, rdb redis.UniversalClient) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	cfg, err := rdb.ConfigGet(ctx, "*").Result()
	if err != nil {
		return nil // CONFIG may be disabled (managed Redis): the deployment checklist covers it
	}
	var errs []error
	if p := cfg["maxmemory-policy"]; p != "" && p != "noeviction" {
		errs = append(errs, fmt.Errorf("queue: redis maxmemory-policy is %q, asynq needs noeviction", p))
	}
	if a := cfg["appendonly"]; a != "" && a != "yes" {
		errs = append(errs, errors.New("queue: redis appendonly is off, queued tasks are lost on restart"))
	}
	return errors.Join(errs...)
}

// Client is the asynq Enqueuer. It shares the caller's Redis connection
// pool: Close does not close it.
type Client struct {
	c   *asynq.Client
	log *slog.Logger
}

// NewClient returns an Enqueuer on rdb.
func NewClient(rdb redis.UniversalClient, log *slog.Logger) *Client {
	return &Client{c: asynq.NewClientFromRedisClient(rdb), log: log}
}

// Close releases the asynq client (not the shared Redis pool).
func (c *Client) Close() error { return c.c.Close() }

// Enqueue implements Enqueuer.
func (c *Client) Enqueue(ctx context.Context, typ string, payload any, dedupID string, opts ...Option) error {
	task, aopts, err := build(typ, payload, dedupID, opts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, enqueueTimeout)
	defer cancel()
	_, err = c.c.EnqueueContext(ctx, task, aopts...)
	if errors.Is(err, asynq.ErrTaskIDConflict) || errors.Is(err, asynq.ErrDuplicateTask) {
		return nil // already queued or completed within the retention window: idempotent
	}
	if err != nil {
		return fmt.Errorf("queue: enqueue %s: %w", typ, err)
	}
	return nil
}

// AfterCommit implements Enqueuer.
func (c *Client) AfterCommit(ctx context.Context, typ string, payload any, dedupID string, opts ...Option) {
	afterCommit(ctx, c, c.log, typ, payload, dedupID, opts)
}

// afterCommit is the shared AfterCommit of every Enqueuer (05 §8.1).
func afterCommit(ctx context.Context, e Enqueuer, log *slog.Logger, typ string, payload any, dedupID string, opts []Option) {
	db.AfterCommit(ctx, func(ctx context.Context) {
		if err := e.Enqueue(ctx, typ, payload, dedupID, opts...); err != nil && log != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "after-commit enqueue failed",
				slog.String("task", typ), slog.String("err", err.Error()))
		}
	})
}

// build turns an Enqueue call into an asynq task and its options.
func build(typ string, payload any, dedupID string, opts []Option) (*asynq.Task, []asynq.Option, error) {
	if typ == "" {
		return nil, nil, errors.New("queue: empty task type")
	}
	o, err := resolve(opts)
	if err != nil {
		return nil, nil, err
	}
	b, err := encode(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("queue: encode %s: %w", typ, err)
	}
	aopts := []asynq.Option{
		asynq.Queue(o.queue),
		asynq.MaxRetry(o.maxRetry),
		asynq.Timeout(o.timeout),
		asynq.Retention(Retention),
	}
	if o.processIn > 0 {
		aopts = append(aopts, asynq.ProcessIn(o.processIn))
	}
	if dedupID != "" {
		aopts = append(aopts, asynq.TaskID(dedupID))
	}
	return asynq.NewTask(typ, b), aopts, nil
}

// encode marshals a payload; []byte and json.RawMessage pass through.
func encode(payload any) ([]byte, error) {
	var b []byte
	switch p := payload.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		b = p
	case []byte:
		b = p
	default:
		var err error
		if b, err = json.Marshal(p); err != nil {
			return nil, err
		}
	}
	if len(b) > maxPayloadBytes {
		return nil, fmt.Errorf("payload of %d bytes exceeds %d (carry IDs only)", len(b), maxPayloadBytes)
	}
	return b, nil
}
