package queue

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Job is a scheduled job (01 §7). The scheduler enqueues a task of type
// Name (no payload) on Cron, interpreted in Europe/Istanbul; the worker runs
// Handle. Jobs work in batches (≤ 1000 rows per transaction), are
// idempotent and read retention periods from kvkk.retention_policies, not
// from code (01 §7).
type Job struct {
	Name     string        // "<module>.<verb>[_<object>]", e.g. "outbox.cleanup"
	Cron     string        // "10 4 * * *" or "@every 10s" (Europe/Istanbul)
	Queue    string        // Critical | Default | Low (01 §7 column)
	Unique   time.Duration // mandatory, shorter than the period: replicas must not fire twice (R-002 §3e)
	MaxRetry int           // 0 → 3: the next run repeats the work anyway
	Timeout  time.Duration // 0 → DefaultTimeout
	Handle   func(ctx context.Context) error
}

// Task is a handler for tasks that modules enqueue themselves (mechanism A
// without a topic, provider polling). Outbox subscriptions are registered in
// outbox.Registry instead.
type Task struct {
	Name   string // the task type
	Handle HandlerFunc
}

// JobMaxRetry is the retry count of a job without MaxRetry.
const JobMaxRetry = 3

var nameRe = regexp.MustCompile(`^[a-z][a-z_]*\.[a-z][a-z_]*$`)

// ValidName reports whether s is a valid task type "<module>.<verb>_<object>".
func ValidName(s string) bool { return nameRe.MatchString(s) }

// cronParser is the parser of asynq's scheduler (robfig/cron standard
// fields plus descriptors such as @every).
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Validate checks a job; Unique must be at least one second and shorter
// than the shortest gap between two runs.
func (j Job) Validate() error {
	var errs []error
	if !ValidName(j.Name) {
		errs = append(errs, fmt.Errorf("job %q: name must be <module>.<verb>_<object>", j.Name))
	}
	if _, ok := Weights()[j.Queue]; !ok {
		errs = append(errs, fmt.Errorf("job %q: unknown queue %q", j.Name, j.Queue))
	}
	if j.Handle == nil {
		errs = append(errs, fmt.Errorf("job %q: nil handler", j.Name))
	}
	if j.MaxRetry < 0 || j.Timeout < 0 {
		errs = append(errs, fmt.Errorf("job %q: negative MaxRetry or Timeout", j.Name))
	}
	period, err := shortestPeriod(j.Cron)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("job %q: cron %q: %w", j.Name, j.Cron, err))
	case j.Unique < minUniqueTTL:
		errs = append(errs, fmt.Errorf("job %q: Unique is mandatory (≥ 1s): asynq schedulers on several replicas fire twice without it", j.Name))
	case j.Unique >= period:
		errs = append(errs, fmt.Errorf("job %q: Unique %s must be shorter than the period %s", j.Name, j.Unique, period))
	}
	return errors.Join(errs...)
}

// shortestPeriod returns the smallest gap between the next activations of
// spec (checked over one year from a fixed instant, so monthly jobs see
// their 28-day February gap).
func shortestPeriod(spec string) (time.Duration, error) {
	s, err := cronParser.Parse(spec)
	if err != nil {
		return 0, err
	}
	t := s.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	shortest := time.Duration(1<<63 - 1)
	for range 400 {
		n := s.Next(t)
		if n.IsZero() {
			break
		}
		shortest = min(shortest, n.Sub(t))
		t = n
	}
	return shortest, nil
}

// Registry collects the jobs and task handlers of every module (their
// tasks(c) in cmd/koro). Registration errors are collected and reported by
// Err when the container is built, so a bad job stops the process at start.
type Registry struct {
	mu    sync.Mutex
	jobs  []Job
	tasks []Task
	names map[string]bool
	errs  []error
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{names: map[string]bool{}} }

// Schedule registers a scheduled job.
func (r *Registry) Schedule(j Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := j.Validate(); err != nil {
		r.errs = append(r.errs, err)
		return
	}
	if !r.claim(j.Name) {
		return
	}
	r.jobs = append(r.jobs, j)
}

// Handle registers a task handler.
func (r *Registry) Handle(t Task) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !ValidName(t.Name):
		r.errs = append(r.errs, fmt.Errorf("task %q: name must be <module>.<verb>_<object>", t.Name))
		return
	case t.Handle == nil:
		r.errs = append(r.errs, fmt.Errorf("task %q: nil handler", t.Name))
		return
	}
	if !r.claim(t.Name) {
		return
	}
	r.tasks = append(r.tasks, t)
}

func (r *Registry) claim(name string) bool {
	if r.names[name] {
		r.errs = append(r.errs, fmt.Errorf("task type %q registered twice", name))
		return false
	}
	r.names[name] = true
	return true
}

// Jobs returns the registered jobs in registration order.
func (r *Registry) Jobs() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.jobs)
}

// Tasks returns the registered task handlers in registration order.
func (r *Registry) Tasks() []Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.tasks)
}

// Err returns every registration error.
func (r *Registry) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(r.errs...)
}
