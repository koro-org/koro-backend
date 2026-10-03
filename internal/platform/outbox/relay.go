package outbox

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"koro/internal/platform/obs"
)

// RelayConfig tunes the relay (config.Outbox).
type RelayConfig struct {
	Batch        int           // rows per transaction (default 100)
	Idle         time.Duration // sleep when nothing is due (default 1 s; 01 §7)
	BackoffMin   time.Duration // delay after the first failure (default 1 s)
	BackoffMax   time.Duration // back-off cap (default 10 min)
	BatchTimeout time.Duration // bound of one batch transaction (default 20 s)
}

// Relay defaults.
const (
	DefaultBatch        = 100
	DefaultIdle         = time.Second
	DefaultBackoffMin   = time.Second
	DefaultBackoffMax   = 10 * time.Minute
	DefaultBatchTimeout = 20 * time.Second
	// AlarmAttempts: a row failing this often is logged at ERROR with
	// alarm=true (metrics koro_outbox_*: BE-058).
	AlarmAttempts = 10
	lastErrorMax  = 500 // characters of outbox.last_error (no payload, no PII)
)

func (c RelayConfig) withDefaults() RelayConfig {
	if c.Batch <= 0 {
		c.Batch = DefaultBatch
	}
	if c.Idle <= 0 {
		c.Idle = DefaultIdle
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = DefaultBackoffMin
	}
	if c.BackoffMax < c.BackoffMin {
		c.BackoffMax = max(DefaultBackoffMax, c.BackoffMin)
	}
	if c.BatchTimeout <= 0 {
		c.BatchTimeout = DefaultBatchTimeout
	}
	return c
}

// Relay moves outbox rows to the queue (01 §7 outbox.relay; 05 §8.3). Run
// it once per worker; several workers are safe (SKIP LOCKED).
type Relay struct {
	o    *Outbox
	cfg  RelayConfig
	beat atomic.Int64 // clock time (Unix ns) of the last loop iteration
}

// NewRelay returns the relay of o.
func (o *Outbox) NewRelay(cfg RelayConfig) *Relay { return &Relay{o: o, cfg: cfg.withDefaults()} }

// Backoff returns the delay before retry number attempts (≥ 1):
// BackoffMin·2^(attempts-1), capped at BackoffMax.
func (r *Relay) Backoff(attempts int) time.Duration {
	d := r.cfg.BackoffMin
	for i := 1; i < attempts && d < r.cfg.BackoffMax; i++ {
		d *= 2
	}
	return min(d, r.cfg.BackoffMax)
}

// Once relays one batch and returns the number of rows it published.
func (r *Relay) Once(ctx context.Context) (int, error) {
	_, published, err := r.once(ctx)
	return published, err
}

// once returns the rows taken (published or postponed) and published.
func (r *Relay) once(ctx context.Context) (taken, published int, err error) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.BatchTimeout)
	defer cancel()
	d := r.o.d
	err = d.Tx.WithTx(ctx, func(ctx context.Context) error {
		taken, published = 0, 0
		now := d.Clock.Now()
		rows, err := d.Store.Due(ctx, now, r.cfg.Batch)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := r.dispatch(ctx, row); err != nil {
				if ctx.Err() != nil {
					// Shutdown or batch timeout: roll back; tasks already
					// enqueued are deduplicated by their task ID next time.
					return ctx.Err()
				}
				attempts := row.Attempts + 1
				msg := truncate(obs.Scrub(err.Error()), lastErrorMax)
				if err := d.Store.MarkFailed(ctx, row.ID, attempts, msg, now.Add(r.Backoff(attempts))); err != nil {
					return err
				}
				level := slog.LevelWarn
				if attempts >= AlarmAttempts {
					level = slog.LevelError
				}
				d.Log.LogAttrs(ctx, level, "outbox relay failed",
					slog.String("outbox_id", row.ID.String()), slog.String("topic", row.Topic),
					slog.Int("attempts", attempts), slog.Bool("alarm", attempts >= AlarmAttempts), slog.String("err", msg))
				taken++
				continue
			}
			if err := d.Store.MarkPublished(ctx, row.ID, now); err != nil {
				return err
			}
			taken++
			published++
		}
		return nil
	})
	return taken, published, err
}

// dispatch enqueues one task per subscriber of the row's topic. A topic
// without subscribers counts as published.
func (r *Relay) dispatch(ctx context.Context, row Row) error {
	e := row.Envelope()
	for _, s := range r.o.d.Subs.For(row.Topic) {
		if err := r.o.d.Queue.Enqueue(ctx, s.Name, e, TaskID(row.ID, s.Name), s.options()...); err != nil {
			return err
		}
	}
	return nil
}

// Run relays until ctx is cancelled: batches back to back while rows are
// due, an Idle pause otherwise. Errors are logged and retried.
func (r *Relay) Run(ctx context.Context) {
	r.touch()
	for {
		taken, _, err := r.once(ctx)
		r.touch()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.o.d.Log.LogAttrs(ctx, slog.LevelError, "outbox relay batch failed", slog.String("err", obs.Scrub(err.Error())))
		}
		if err == nil && taken == r.cfg.Batch {
			continue
		}
		t := time.NewTimer(r.cfg.Idle)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (r *Relay) touch() { r.beat.Store(r.o.d.Clock.Now().UnixNano()) }

// LastBeat returns the clock time of the last loop iteration (zero before
// Run starts). The worker liveness check (/healthz) compares it with now.
func (r *Relay) LastBeat() time.Time {
	ns := r.beat.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// MaxBeatAge is the liveness threshold of the relay loop: a batch is
// bounded by BatchTimeout and a pause by Idle, so a healthy loop beats far
// more often.
func (r *Relay) MaxBeatAge() time.Duration { return 2*r.cfg.BatchTimeout + r.cfg.Idle + 10*time.Second }

// errNotRunning is reported by Alive before the first beat.
var errNotRunning = errors.New("outbox: relay loop is not running")

// Alive reports whether the relay loop beat within MaxBeatAge of now.
func (r *Relay) Alive(now time.Time) error {
	last := r.LastBeat()
	if last.IsZero() {
		return errNotRunning
	}
	if age := now.Sub(last); age > r.MaxBeatAge() {
		return errors.New("outbox: relay loop stalled for " + age.Round(time.Second).String())
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
