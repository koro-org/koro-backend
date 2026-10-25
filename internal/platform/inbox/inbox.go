// Package inbox is the webhook inbox (01 §1.3, 02 §1.2): provider webhooks
// are written once into webhook_inbox ((provider, event_key) is unique) and
// processed by a worker task, so the HTTP handler answers quickly and a
// redelivered webhook is not processed twice.
//
//   - Receive (webhook handler, outside a transaction): stores the delivery
//     and enqueues "webhook_inbox.process" with task ID
//     "<inbox id>:webhook_inbox.process". If the enqueue fails it returns
//     the error, the handler answers 5xx and the provider's retry re-enqueues
//     the stored, still unprocessed row (isNew=false).
//   - ProcessTask (worker): runs the provider's Processor and marks the row
//     processed in ONE transaction (processors write only to the database;
//     external effects go through the outbox).
//   - PurgeJob (webhook_inbox.purge_payloads, 01 §7): nulls payloads and
//     headers after 90 days, deletes rows after 2 years.
package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// Providers (webhook_inbox_provider_chk).
const (
	ProviderIyzico  = "iyzico"
	ProviderParasut = "parasut"
	ProviderEmail   = "email" // SES / Resend bounce and complaint notifications (04 §2.0)
	ProviderIYS     = "iys"
)

// Providers returns every provider accepted by the table.
func Providers() []string {
	return []string{ProviderIyzico, ProviderParasut, ProviderEmail, ProviderIYS}
}

// ProcessTaskName is the task type of inbox processing (and the SystemActor
// job name; action webhook.inbox.process, 03 §3.7).
const ProcessTaskName = "webhook_inbox.process"

// Errors.
var (
	ErrInTx          = errors.New("inbox: Receive must run outside a transaction (the enqueue follows the commit)")
	ErrInvalid       = errors.New("inbox: invalid webhook delivery")
	ErrNoProcessor   = errors.New("inbox: no processor registered for the provider")
	errPurged        = errors.New("inbox: payload already purged")
	errUnsignedEvent = errors.New("inbox: delivery with an invalid signature is never processed")
)

// strippedHeaders never reach the table (02 §1.2: "Authorization yok").
var strippedHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true, "set-cookie": true,
	"x-api-key": true, "x-auth-token": true,
}

const (
	maxEventKey  = 200 // webhook_inbox_event_key_chk
	lastErrorMax = 500
)

// Event is a stored delivery handed to a Processor.
type Event struct {
	ID         uuid.UUID
	Provider   string
	EventKey   string
	ReceivedAt time.Time
	Headers    map[string]string
	Payload    json.RawMessage
}

// Processor handles the deliveries of one provider. Handle runs in the
// transaction that marks the row processed; it writes only to the database
// (an e-mail, an API call... go through the outbox) and must tolerate a
// repeated delivery of the same provider event under another key.
type Processor struct {
	Provider string
	Queue    string // queue of the processing task (default queue.Default)
	MaxRetry int    // 0 → queue.DefaultMaxRetry; payment/invoice providers queue.MoneyMaxRetry
	Handle   func(ctx context.Context, e Event) error
}

// Row is one webhook_inbox row (02 §1.2).
type Row struct {
	ID              uuid.UUID         `gorm:"type:uuid;primaryKey"`
	Provider        string            `gorm:"column:provider"`
	EventKey        string            `gorm:"column:event_key"`
	ReceivedAt      time.Time         `gorm:"column:received_at"`
	SignatureValid  bool              `gorm:"column:signature_valid"`
	Headers         map[string]string `gorm:"column:headers;serializer:json;type:jsonb"`
	Payload         json.RawMessage   `gorm:"column:payload;type:jsonb"`
	PayloadSHA256   []byte            `gorm:"column:payload_sha256"`
	PayloadPurgedAt *time.Time        `gorm:"column:payload_purged_at"`
	ProcessedAt     *time.Time        `gorm:"column:processed_at"`
	Attempts        int               `gorm:"column:attempts"`
	LastError       *string           `gorm:"column:last_error"`
}

// TableName implements gorm's Tabler.
func (Row) TableName() string { return "webhook_inbox" }

// Store persists inbox rows.
type Store interface {
	// Insert stores r unless (provider, event_key) exists. A row stored
	// with an invalid signature is replaced by a validly signed delivery
	// (a forged first delivery must not suppress the real one).
	// inserted reports a new (or replaced) row; otherwise the existing row
	// is returned.
	Insert(ctx context.Context, r Row) (stored Row, inserted bool, err error)
	// Lock returns the row locked FOR UPDATE in the ctx transaction.
	Lock(ctx context.Context, id uuid.UUID) (Row, error)
	// MarkProcessed sets processed_at and attempts.
	MarkProcessed(ctx context.Context, id uuid.UUID, at time.Time, attempts int) error
	// MarkFailed records a failed attempt.
	MarkFailed(ctx context.Context, id uuid.UUID, attempts int, lastErr string) error
	// PurgePayloads nulls payload and headers of up to limit rows received
	// before the cutoff.
	PurgePayloads(ctx context.Context, before, now time.Time, limit int) (int, error)
	// DeleteOld deletes up to limit rows received before the cutoff.
	DeleteOld(ctx context.Context, before time.Time, limit int) (int, error)
}

// Deps are the dependencies of Inbox.
type Deps struct {
	Store Store
	Tx    db.TxManager
	Queue queue.Enqueuer
	Clock clock.Clock
	Log   *slog.Logger
}

// Inbox receives and processes webhooks.
type Inbox struct {
	d          Deps
	mu         sync.RWMutex
	processors map[string]Processor
}

// New returns the inbox.
func New(d Deps) *Inbox { return &Inbox{d: d, processors: map[string]Processor{}} }

// Register installs the processor of a provider (the provider module's
// wire file, e.g. notify for "email"). One processor per provider.
func (in *Inbox) Register(p Processor) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	switch {
	case !validProvider(p.Provider):
		return fmt.Errorf("%w: unknown provider %q", ErrInvalid, p.Provider)
	case p.Handle == nil:
		return fmt.Errorf("%w: nil processor for %q", ErrInvalid, p.Provider)
	}
	if _, dup := in.processors[p.Provider]; dup {
		return fmt.Errorf("inbox: provider %q already has a processor", p.Provider)
	}
	if p.Queue == "" {
		p.Queue = queue.Default
	}
	if _, ok := queue.Weights()[p.Queue]; !ok {
		return fmt.Errorf("%w: unknown queue %q", ErrInvalid, p.Queue)
	}
	in.processors[p.Provider] = p
	return nil
}

func (in *Inbox) processor(provider string) (Processor, bool) {
	in.mu.RLock()
	defer in.mu.RUnlock()
	p, ok := in.processors[provider]
	return p, ok
}

func validProvider(p string) bool {
	for _, v := range Providers() {
		if p == v {
			return true
		}
	}
	return false
}

// Receive stores a webhook delivery (01 §1.3). eventKey is the provider's
// event ID or a digest of the signed content (1–200 characters). headers
// should hold only the headers needed for processing; credentials are
// dropped anyway. A payload that is not JSON is stored as a JSON string.
// isNew=false means the delivery was already stored: answer 2xx as well.
func (in *Inbox) Receive(ctx context.Context, provider, eventKey string, sigValid bool, headers map[string]string, payload []byte) (isNew bool, err error) {
	if db.InTx(ctx) {
		return false, ErrInTx
	}
	if !validProvider(provider) || eventKey == "" || utf8.RuneCountInString(eventKey) > maxEventKey {
		return false, fmt.Errorf("%w: provider %q, event key of %d characters", ErrInvalid, provider, utf8.RuneCountInString(eventKey))
	}
	sum := sha256.Sum256(payload)
	r := Row{
		ID: uuid.NewV7(), Provider: provider, EventKey: eventKey, ReceivedAt: in.d.Clock.Now(), SignatureValid: sigValid,
		Headers: cleanHeaders(headers), Payload: asJSON(payload), PayloadSHA256: sum[:],
	}
	stored, inserted, err := in.d.Store.Insert(ctx, r)
	if err != nil {
		return false, err
	}
	if stored.SignatureValid && stored.ProcessedAt == nil {
		if err := in.enqueue(ctx, stored); err != nil {
			return inserted, err
		}
	}
	return inserted, nil
}

// enqueue queues the processing task of r (deduplicated by task ID).
func (in *Inbox) enqueue(ctx context.Context, r Row) error {
	opts := []queue.Option{queue.Queue(queue.Default)}
	if p, ok := in.processor(r.Provider); ok {
		opts = []queue.Option{queue.Queue(p.Queue), queue.MaxRetry(p.MaxRetry)}
	}
	return in.d.Queue.Enqueue(ctx, ProcessTaskName, taskPayload{ID: r.ID}, r.ID.String()+":"+ProcessTaskName, opts...)
}

type taskPayload struct {
	ID uuid.UUID `json:"id"`
}

// Task returns the processing task handler for the worker.
func (in *Inbox) Task() queue.Task { return queue.Task{Name: ProcessTaskName, Handle: in.process} }

// process runs one delivery (webhook_inbox.process).
func (in *Inbox) process(ctx context.Context, payload []byte) error {
	var tp taskPayload
	if err := json.Unmarshal(payload, &tp); err != nil || tp.ID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("inbox: bad task payload: %w", errors.Join(err, ErrInvalid)))
	}
	if err := authz.Can(ctx, authz.SystemActor(ProcessTaskName), authz.WebhookInboxProcess, authz.Resource{}); err != nil {
		return queue.Permanent(err)
	}
	var row Row
	err := in.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if row, err = in.d.Store.Lock(ctx, tp.ID); err != nil {
			return err
		}
		switch {
		case row.ProcessedAt != nil:
			return nil // already done (redelivered task)
		case !row.SignatureValid:
			return queue.Permanent(errUnsignedEvent)
		case row.PayloadPurgedAt != nil:
			return queue.Permanent(errPurged)
		}
		p, ok := in.processor(row.Provider)
		if !ok {
			return queue.Permanent(fmt.Errorf("%w: %s", ErrNoProcessor, row.Provider))
		}
		ev := Event{ID: row.ID, Provider: row.Provider, EventKey: row.EventKey, ReceivedAt: row.ReceivedAt,
			Headers: row.Headers, Payload: row.Payload}
		if err := p.Handle(ctx, ev); err != nil {
			return err
		}
		return in.d.Store.MarkProcessed(ctx, row.ID, in.d.Clock.Now(), row.Attempts+1)
	})
	switch {
	case err == nil:
		return nil
	case row.ID == uuid.Nil() && db.IsNotFound(err):
		return queue.Permanent(err) // the row is gone (deleted after its retention)
	case row.ID == uuid.Nil():
		return err
	}
	// The processing transaction rolled back: record the attempt separately.
	msg := truncate(obs.Scrub(err.Error()), lastErrorMax)
	if ctx.Err() == nil {
		if mErr := in.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			return in.d.Store.MarkFailed(ctx, row.ID, row.Attempts+1, msg)
		}); mErr != nil {
			in.d.Log.LogAttrs(ctx, slog.LevelWarn, "inbox: record failure", slog.String("err", mErr.Error()))
		}
	}
	return err
}

// Purge job (01 §7: webhook_inbox.purge_payloads, daily 04:20, low).
const (
	PurgeJobName            = "webhook_inbox.purge_payloads"
	purgeCron               = "20 4 * * *"
	purgeUnique             = 23 * time.Hour
	purgeBatch              = 1000
	DefaultPayloadRetention = 90 * 24 * time.Hour  // retention_policies webhook_payload
	DefaultRowRetention     = 730 * 24 * time.Hour // retention_policies webhook_row (2 years)
)

// PurgeJob nulls old payloads and deletes old rows, in batches. The
// retention functions read retention_policies (or fall back to config).
func (in *Inbox) PurgeJob(payloadRetention, rowRetention func(ctx context.Context) (time.Duration, error)) queue.Job {
	return queue.Job{
		Name: PurgeJobName, Cron: purgeCron, Queue: queue.Low, Unique: purgeUnique,
		Handle: func(ctx context.Context) error {
			keepPayload, err := payloadRetention(ctx)
			if err != nil {
				return err
			}
			keepRow, err := rowRetention(ctx)
			if err != nil {
				return err
			}
			now := in.d.Clock.Now()
			purged, err := db.Drain(ctx, purgeBatch, func(ctx context.Context) (int, error) {
				return in.d.Store.PurgePayloads(ctx, now.Add(-keepPayload), now, purgeBatch)
			})
			if err != nil {
				return err
			}
			deleted, err := db.Drain(ctx, purgeBatch, func(ctx context.Context) (int, error) {
				return in.d.Store.DeleteOld(ctx, now.Add(-keepRow), purgeBatch)
			})
			if err != nil {
				return err
			}
			in.d.Log.LogAttrs(ctx, slog.LevelInfo, "webhook inbox purge done",
				slog.Int("payloads_purged", purged), slog.Int("rows_deleted", deleted))
			return nil
		},
	}
}

func cleanHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if !strippedHeaders[strings.ToLower(k)] {
			out[k] = v
		}
	}
	return out
}

// asJSON keeps a JSON payload as it is and wraps anything else into a JSON
// string (form-encoded or XML webhooks).
func asJSON(payload []byte) json.RawMessage {
	if json.Valid(payload) {
		return payload
	}
	b, _ := json.Marshal(string(payload))
	return b
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
