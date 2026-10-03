// Package queue is the asynq layer of Koro (05 §8.5, 01 §8; ADR-0009):
//
//   - Enqueuer: Enqueue (deduplicated by task ID, 24 h retention) and
//     AfterCommit (mechanism A: enqueue after the transaction commits, never
//     on rollback; best effort). Client is the asynq implementation, Mem the
//     recording fake of service tests.
//   - Registry: the scheduled jobs (Job, Unique mandatory) and plain task
//     handlers (Task) that modules declare in their wire file.
//   - Server: the worker (weighted queues critical/default/low = 6/3/1,
//     retry policy, dead letters → OnDeadLetter), Mux (panic recovery, task
//     ID in the log context, scrubbed errors) and Scheduler
//     (Europe/Istanbul).
//
// Modules never import asynq: options, the permanent-error marker and the
// handler types are declared here, so the queue backend stays replaceable.
//
// Asynq traps (R-002 §3e, §5.3), handled here:
//   - a task ID is free again once the task completes unless the task is
//     retained: every Enqueue sets Retention(24 h);
//   - Scheduler replicas are not coordinated: every Job carries
//     asynq.Unique(<shorter than its period>) and WORKER_SCHEDULER_ENABLED
//     can switch the scheduler off on all but one replica;
//   - Server.Run installs its own signal handler: the worker uses
//     Start/Shutdown.
package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
)

// Queues and their weights (01 §8). Weighted, not strict priority: low
// tasks still run while critical ones wait (R-002 §3e).
const (
	Critical = "critical" // ticket e-mail, payment/payout/invoice, consent/İYS, KVKK, audit chain
	Default  = "default"  // other outbox events, user notifications, state machine jobs
	Low      = "low"      // talent, reconciliation, retention, cleanup
)

// Weights returns the queue weights of the worker (01 §8).
func Weights() map[string]int { return map[string]int{Critical: 6, Default: 3, Low: 1} }

// Retry and lifetime defaults (01 §8, 05 §8.5).
const (
	DefaultMaxRetry  = 10              // exponential, at most MaxRetryDelay between attempts
	MoneyMaxRetry    = 25              // payment, payout and invoice handlers
	DefaultTimeout   = 2 * time.Minute // per attempt; the handler ctx is cancelled after it
	Retention        = 24 * time.Hour  // completed tasks keep their ID: the deduplication window
	MaxRetryDelay    = 6 * time.Hour   // cap of the retry back-off (01 §8)
	maxPayloadBytes  = 64 << 10        // payloads carry IDs only (05 §8.4-1: < 1 KB); this is a hard stop
	enqueueTimeout   = 5 * time.Second // one Redis round trip of Enqueue
	pingTimeout      = 2 * time.Second // Redis ping at start and in /readyz
	redisDialTimeout = 3 * time.Second // connection establishment
	redisRWTimeout   = 3 * time.Second // read/write deadline of one command
	redisPoolTimeout = 4 * time.Second // waiting for a free connection
	minUniqueTTL     = 1 * time.Second // asynq rejects shorter Unique TTLs
)

// ErrPermanent marks a failure that retrying cannot fix (undecodable
// payload, missing record): the task is archived at once (05 §8.4-3). Wrap
// it with Permanent.
var ErrPermanent = asynq.SkipRetry

// Permanent marks err as permanent: the task is not retried.
func Permanent(err error) error {
	if err == nil || errors.Is(err, asynq.SkipRetry) {
		return err
	}
	return fmt.Errorf("%w: %w", asynq.SkipRetry, err)
}

// IsPermanent reports whether err is marked permanent.
func IsPermanent(err error) bool { return errors.Is(err, asynq.SkipRetry) }

// HandlerFunc processes one task payload. Return nil when the task is done
// or meaningless now (05 §8.4-2), a Permanent error when retrying cannot
// help, any other error to retry with back-off. Handlers return promptly
// when ctx is cancelled (shutdown: the task goes back to the queue).
type HandlerFunc func(ctx context.Context, payload []byte) error

// Option configures one enqueued task.
type Option func(*options)

type options struct {
	queue     string
	maxRetry  int
	timeout   time.Duration
	processIn time.Duration
}

// Queue selects the queue (Critical, Default, Low).
func Queue(name string) Option { return func(o *options) { o.queue = name } }

// MaxRetry sets the number of retries; n ≤ 0 keeps DefaultMaxRetry.
func MaxRetry(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxRetry = n
		}
	}
}

// Timeout bounds one attempt; d ≤ 0 keeps DefaultTimeout.
func Timeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// ProcessIn delays the first attempt by d (e.g. provider polling).
func ProcessIn(d time.Duration) Option { return func(o *options) { o.processIn = d } }

func resolve(opts []Option) (options, error) {
	o := options{queue: Default, maxRetry: DefaultMaxRetry, timeout: DefaultTimeout}
	for _, f := range opts {
		f(&o)
	}
	if _, ok := Weights()[o.queue]; !ok {
		return o, fmt.Errorf("queue: unknown queue %q", o.queue)
	}
	return o, nil
}

// Enqueuer puts tasks on the queue. Services depend on it (mechanism A);
// the outbox relay uses it to fan events out.
type Enqueuer interface {
	// Enqueue adds a task of type typ with the JSON of payload. A non-empty
	// dedupID becomes the asynq task ID: a second enqueue with the same ID
	// within the retention window is a no-op that returns nil.
	Enqueue(ctx context.Context, typ string, payload any, dedupID string, opts ...Option) error
	// AfterCommit enqueues once the transaction carried by ctx commits (at
	// once without a transaction; never after a rollback). Failures are
	// logged, not returned: mechanism A is best effort and nightly
	// reconciliation repairs losses (01 §6.1).
	AfterCommit(ctx context.Context, typ string, payload any, dedupID string, opts ...Option)
}
