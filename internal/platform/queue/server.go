package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"koro/internal/platform/clock"
	"koro/internal/platform/obs"
)

// Mux routes tasks to handlers by type. It recovers panics, puts the task ID
// into the log context and scrubs handler errors before asynq stores them
// in Redis (asynqmon shows them; 05 §11.2).
type Mux struct {
	handlers map[string]HandlerFunc
	log      *slog.Logger
}

// NewMux returns an empty mux.
func NewMux(log *slog.Logger) *Mux { return &Mux{handlers: map[string]HandlerFunc{}, log: log} }

// Handle registers h for task type name; a duplicate type is an error.
func (m *Mux) Handle(name string, h HandlerFunc) error {
	if h == nil || name == "" {
		return fmt.Errorf("queue: invalid handler for %q", name)
	}
	if _, dup := m.handlers[name]; dup {
		return fmt.Errorf("queue: task type %q registered twice", name)
	}
	m.handlers[name] = h
	return nil
}

// Types returns the registered task types, sorted.
func (m *Mux) Types() []string { return slices.Sorted(maps.Keys(m.handlers)) }

// ProcessTask implements asynq.Handler.
func (m *Mux) ProcessTask(ctx context.Context, t *asynq.Task) (err error) {
	h, ok := m.handlers[t.Type()]
	if !ok {
		return Permanent(fmt.Errorf("queue: no handler for task type %q", t.Type()))
	}
	if id, ok := asynq.GetTaskID(ctx); ok {
		ctx = obs.WithTaskID(ctx, id)
	}
	defer func() {
		if r := recover(); r != nil {
			m.log.LogAttrs(ctx, slog.LevelError, "task panicked",
				slog.String("task", t.Type()), slog.String("panic", obs.Scrub(fmt.Sprint(r))),
				slog.String("stack", string(debug.Stack())))
			err = fmt.Errorf("queue: task %s panicked", t.Type())
		}
	}()
	return scrubbed(h(ctx, t.Payload()))
}

// scrubbedError carries a PII-scrubbed message and keeps the original chain
// for errors.Is (SkipRetry, context.Canceled).
type scrubbedError struct {
	msg   string
	cause error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.cause }

func scrubbed(err error) error {
	if err == nil {
		return nil
	}
	return &scrubbedError{msg: obs.Scrub(err.Error()), cause: err}
}

// DeadLetter describes a task that asynq archived: retries exhausted or a
// permanent error (01 §8: "ölü mektup").
type DeadLetter struct {
	Queue    string
	Type     string
	TaskID   string
	Retried  int
	MaxRetry int
	Err      string // scrubbed
}

// ServerConfig configures the worker server.
type ServerConfig struct {
	Concurrency     int           // parallel tasks (default 10)
	ShutdownTimeout time.Duration // unfinished tasks go back to the queue after it (default 25 s)
	Log             *slog.Logger
	// OnDeadLetter is called for every archived task, in addition to the
	// ERROR log line. nil → log only. The alarm (GlitchTip) adapter of
	// BE-055/BE-058 plugs in here; critical-queue dead letters carry
	// alarm=true in the log.
	OnDeadLetter func(ctx context.Context, d DeadLetter)
}

// Server is the asynq worker server.
type Server struct {
	srv *asynq.Server
	mux *Mux
}

// NewServer returns a worker server on rdb (shared pool) serving mux. ctx
// is the base of asynq's own log lines (values only, never cancelled).
func NewServer(ctx context.Context, rdb redis.UniversalClient, mux *Mux, cfg ServerConfig) *Server {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 10
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 25 * time.Second
	}
	log := cfg.Log
	srv := asynq.NewServerFromRedisClient(rdb, asynq.Config{
		Concurrency:     cfg.Concurrency,
		Queues:          Weights(),
		ShutdownTimeout: cfg.ShutdownTimeout,
		Logger:          slogAdapter{ctx: context.WithoutCancel(ctx), log: log},
		LogLevel:        asynq.WarnLevel,
		RetryDelayFunc:  RetryDelay,
		// Shutdown cancels running handlers: that is not a failure (the task
		// goes back to the queue without spending a retry; R-002 §3e).
		IsFailure:    func(err error) bool { return !errors.Is(err, context.Canceled) },
		ErrorHandler: asynq.ErrorHandlerFunc(errorHandler(log, cfg.OnDeadLetter)),
	})
	return &Server{srv: srv, mux: mux}
}

// RetryDelay is asynq's default polynomial back-off capped at MaxRetryDelay
// (01 §8: exponential, at most 6 hours).
func RetryDelay(n int, err error, t *asynq.Task) time.Duration {
	return min(asynq.DefaultRetryDelayFunc(n, err, t), MaxRetryDelay)
}

// errorHandler logs every failed attempt and reports dead letters.
func errorHandler(log *slog.Logger, onDead func(context.Context, DeadLetter)) func(context.Context, *asynq.Task, error) {
	return func(ctx context.Context, t *asynq.Task, err error) {
		retried, _ := asynq.GetRetryCount(ctx)
		maxRetry, _ := asynq.GetMaxRetry(ctx)
		queue, _ := asynq.GetQueueName(ctx)
		id, _ := asynq.GetTaskID(ctx)
		attrs := []slog.Attr{
			slog.String("task", t.Type()), slog.String("queue", queue), slog.String("task_id", id),
			slog.Int("retried", retried), slog.Int("max_retry", maxRetry), slog.String("err", obs.Scrub(err.Error())),
		}
		if errors.Is(err, context.Canceled) {
			return // shutdown: the task is re-queued
		}
		if retried < maxRetry && !errors.Is(err, asynq.SkipRetry) {
			log.LogAttrs(ctx, slog.LevelWarn, "task failed, will retry", attrs...)
			return
		}
		attrs = append(attrs, slog.Bool("alarm", queue == Critical))
		log.LogAttrs(ctx, slog.LevelError, "task archived (dead letter)", attrs...)
		if onDead != nil {
			onDead(ctx, DeadLetter{Queue: queue, Type: t.Type(), TaskID: id, Retried: retried, MaxRetry: maxRetry,
				Err: obs.Scrub(err.Error())})
		}
	}
}

// Start starts processing (non-blocking). Never use asynq's Run: it
// installs its own signal handler (R-002 §5.3).
func (s *Server) Start() error {
	if err := s.srv.Start(s.mux); err != nil {
		return fmt.Errorf("queue: start server: %w", err)
	}
	return nil
}

// Stop stops pulling new tasks; running ones continue.
func (s *Server) Stop() { s.srv.Stop() }

// Shutdown waits up to ShutdownTimeout for running tasks; unfinished tasks
// go back to the queue without spending a retry.
func (s *Server) Shutdown() { s.srv.Shutdown() }

// Ping checks the server's Redis connection.
func (s *Server) Ping() error { return s.srv.Ping() }

// Scheduler enqueues the registered jobs on their cron (Europe/Istanbul).
// Every replica running a scheduler fires every job; Job.Unique makes the
// second enqueue a no-op (R-002 §3e).
type Scheduler struct {
	s   *asynq.Scheduler
	log *slog.Logger
}

// NewScheduler returns a scheduler on rdb (shared pool). ctx is the base
// of its log lines (values only, never cancelled).
func NewScheduler(ctx context.Context, rdb redis.UniversalClient, log *slog.Logger) *Scheduler {
	base := context.WithoutCancel(ctx)
	s := asynq.NewSchedulerFromRedisClient(rdb, &asynq.SchedulerOpts{
		Location: clock.Istanbul(),
		Logger:   slogAdapter{ctx: base, log: log},
		LogLevel: asynq.WarnLevel,
		PostEnqueueFunc: func(info *asynq.TaskInfo, err error) {
			// A duplicate is the Unique lock of another replica: expected.
			if err != nil && !errors.Is(err, asynq.ErrDuplicateTask) && !errors.Is(err, asynq.ErrTaskIDConflict) {
				log.LogAttrs(base, slog.LevelError, "scheduled enqueue failed", slog.String("err", obs.Scrub(err.Error())))
			}
		},
	})
	return &Scheduler{s: s, log: log}
}

// Register adds a job (validated again: Unique is mandatory).
func (s *Scheduler) Register(j Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	maxRetry := j.MaxRetry
	if maxRetry == 0 {
		maxRetry = JobMaxRetry
	}
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	_, err := s.s.Register(j.Cron, asynq.NewTask(j.Name, nil),
		asynq.Queue(j.Queue), asynq.Unique(j.Unique), asynq.MaxRetry(maxRetry), asynq.Timeout(timeout))
	if err != nil {
		return fmt.Errorf("queue: schedule %s: %w", j.Name, err)
	}
	return nil
}

// Start starts the scheduler (non-blocking).
func (s *Scheduler) Start() error {
	if err := s.s.Start(); err != nil {
		return fmt.Errorf("queue: start scheduler: %w", err)
	}
	return nil
}

// Shutdown stops scheduling.
func (s *Scheduler) Shutdown() { s.s.Shutdown() }

// JobHandler adapts a Job to the mux (jobs carry no payload).
func JobHandler(j Job) HandlerFunc {
	return func(ctx context.Context, _ []byte) error { return j.Handle(ctx) }
}

// slogAdapter routes asynq's own log lines to slog (asynq's Logger has no
// context: ctx is the process base context).
type slogAdapter struct {
	ctx context.Context
	log *slog.Logger
}

func (a slogAdapter) write(level slog.Level, args []any) {
	msg := strings.TrimSpace(fmt.Sprint(args...))
	if strings.Contains(msg, "connection is shared") {
		level = slog.LevelDebug // Close on a shared pool is a no-op by design (we close Redis ourselves)
	}
	a.log.LogAttrs(a.ctx, level, "asynq: "+obs.Scrub(msg))
}

func (a slogAdapter) Debug(args ...any) { a.write(slog.LevelDebug, args) }
func (a slogAdapter) Info(args ...any)  { a.write(slog.LevelInfo, args) }
func (a slogAdapter) Warn(args ...any)  { a.write(slog.LevelWarn, args) }
func (a slogAdapter) Error(args ...any) { a.write(slog.LevelError, args) }
func (a slogAdapter) Fatal(args ...any) { a.write(slog.LevelError, args) }
