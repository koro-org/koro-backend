package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// Handler consumes one event. It must be idempotent (01 §6.1): the key is
// e.ID; a unique constraint, notification_deliveries.dedupe_key or a state
// transition guard makes a second run harmless (05 §8.4). Handlers act as
// authz.SystemActor(<subscription name>).
type Handler func(ctx context.Context, e domainevent.Envelope) error

// Subscription binds a handler to a topic. Name is the asynq task type
// "<module>.<verb>_<object>" (01 §8, e.g. events.send_ticket_email); one
// task per (event, subscription) is enqueued with task ID
// "<event id>:<name>".
type Subscription struct {
	Topic    string
	Name     string
	Queue    string        // default queue.Default
	MaxRetry int           // 0 → queue.DefaultMaxRetry; money handlers queue.MoneyMaxRetry
	Timeout  time.Duration // 0 → queue.DefaultTimeout
	Handle   Handler
}

// Option configures a subscription.
type Option func(*Subscription)

// Queue selects the queue of the subscription's tasks.
func Queue(name string) Option { return func(s *Subscription) { s.Queue = name } }

// MaxRetry sets the retry count of the subscription's tasks.
func MaxRetry(n int) Option { return func(s *Subscription) { s.MaxRetry = n } }

// Timeout bounds one attempt of the subscription's tasks.
func Timeout(d time.Duration) Option { return func(s *Subscription) { s.Timeout = d } }

// TaskID is the deduplication ID of the task of event id for subscription
// name (05 §8.3): stable across relay retries.
func TaskID(id fmt.Stringer, name string) string { return id.String() + ":" + name }

// options returns the queue options of the subscription's tasks.
func (s Subscription) options() []queue.Option {
	return []queue.Option{queue.Queue(s.Queue), queue.MaxRetry(s.MaxRetry), queue.Timeout(s.Timeout)}
}

// HandleTask decodes the envelope of a task and runs the handler; it is the
// queue.HandlerFunc that `koro worker` mounts under s.Name. An undecodable
// payload is a permanent failure.
func (s Subscription) HandleTask(ctx context.Context, payload []byte) error {
	var e domainevent.Envelope
	if err := json.Unmarshal(payload, &e); err != nil {
		return queue.Permanent(fmt.Errorf("outbox: %s: decode envelope: %w", s.Name, err))
	}
	if e.Topic != s.Topic {
		return queue.Permanent(fmt.Errorf("outbox: %s: got topic %q, subscribed to %q", s.Name, e.Topic, s.Topic))
	}
	if e.RequestID != "" && obs.RequestID(ctx) == "" {
		ctx = obs.WithRequestID(ctx, e.RequestID) // correlate worker logs with the originating request
	}
	return s.Handle(ctx, e)
}

// Registry holds the subscriptions of every module. Modules subscribe in
// their wire file (tasks(c)); errors are collected and reported by Err when
// the container is built.
type Registry struct {
	mu      sync.RWMutex
	subs    []Subscription
	byTopic map[string][]Subscription
	names   map[string]bool
	errs    []error
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byTopic: map[string][]Subscription{}, names: map[string]bool{}}
}

// Subscribe registers handler name for topic (01 §1.3).
func (r *Registry) Subscribe(topic, name string, h Handler, opts ...Option) {
	s := Subscription{Topic: topic, Name: name, Queue: queue.Default, Handle: h}
	for _, o := range opts {
		o(&s)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	if _, ok := domainevent.Lookup(topic); !ok {
		errs = append(errs, fmt.Errorf("subscription %q: %w: %q", name, domainevent.ErrUnknownTopic, topic))
	}
	if !queue.ValidName(name) {
		errs = append(errs, fmt.Errorf("subscription %q: name must be <module>.<verb>_<object>", name))
	}
	if _, ok := queue.Weights()[s.Queue]; !ok {
		errs = append(errs, fmt.Errorf("subscription %q: unknown queue %q", name, s.Queue))
	}
	if h == nil || s.MaxRetry < 0 || s.Timeout < 0 {
		errs = append(errs, fmt.Errorf("subscription %q: nil handler or negative MaxRetry/Timeout", name))
	}
	if r.names[name] {
		errs = append(errs, fmt.Errorf("subscription %q registered twice", name))
	}
	if len(errs) > 0 {
		r.errs = append(r.errs, errs...)
		return
	}
	r.names[name] = true
	r.subs = append(r.subs, s)
	r.byTopic[topic] = append(r.byTopic[topic], s)
}

// For returns the subscriptions of topic.
func (r *Registry) For(topic string) []Subscription {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.byTopic[topic])
}

// All returns every subscription in registration order.
func (r *Registry) All() []Subscription {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.subs)
}

// Err returns every registration error.
func (r *Registry) Err() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return errors.Join(r.errs...)
}
