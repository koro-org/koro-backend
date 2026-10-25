// Package idempotency implements Idempotency-Key (04 §1.8, 05 §10.4,
// 06 H-5):
//
//   - Middleware (HTTP layer) only parses and validates the header of
//     operations with x-koro-idempotency (8–255 characters,
//     ^[A-Za-z0-9_-]+$; missing on a "required" operation → 422) and puts
//     the key with its scope "<actor_user_id>:<METHOD> <path template>"
//     into the context.
//   - Do (service layer) reserves the key and runs the business write in the
//     SAME transaction: the key and the business data commit or roll back
//     together. A concurrent request with the same key waits on the unique
//     index until the first transaction ends, then replays its response.
//     A failed (rolled back) attempt does not consume the key.
//   - Same key + same request hash → the stored result is replayed and the
//     response carries Idempotent-Replayed: true. Same key + different
//     hash → 422 idempotency.key_reused.
//   - CleanupJob (idempotency.cleanup, 01 §7) deletes expired keys.
//
// Anonymous requests are never deduplicated (no stable owner for the scope).
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// Errors (04 §1.8).
var (
	ErrKeyReused = problem.New(problem.Invalid, "idempotency.key_reused",
		"Idempotency key was used with a different request")
	// ErrInProgress is defensive: with the key written in the business
	// transaction an unfinished key is never visible to another request.
	ErrInProgress = problem.New(problem.Conflict, "idempotency.in_progress",
		"A request with this idempotency key is in progress")
)

// Durations (02 §1.3).
const (
	DefaultTTL = 24 * time.Hour   // expires_at = created_at + 24 h
	lockFor    = 30 * time.Second // locked_until of an in_progress key
)

// Status values of idempotency_keys.status.
const (
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

// Record is a stored key.
type Record struct {
	RequestHash    []byte
	Status         string
	ResponseStatus int
	ResponseBody   []byte
}

// Store persists keys in the transaction carried by ctx.
type Store interface {
	// Reserve inserts the key as in_progress, or takes over an expired
	// row. reserved=false returns the live row (waiting for a concurrent
	// transaction holding the same key to finish).
	Reserve(ctx context.Context, scope, key string, hash []byte, now, lockedUntil, expiresAt time.Time) (rec Record, reserved bool, err error)
	// Complete stores the response of a reserved key.
	Complete(ctx context.Context, scope, key string, status int, body []byte) error
	// DeleteExpired deletes up to limit keys with expires_at < now.
	DeleteExpired(ctx context.Context, now time.Time, limit int) (int, error)
}

// Deps are the dependencies of Keeper.
type Deps struct {
	Tx    db.TxManager
	Store Store
	Clock clock.Clock
	Log   *slog.Logger
	TTL   time.Duration // 0 → DefaultTTL
}

// Keeper runs idempotent service calls.
type Keeper struct{ d Deps }

// New returns a Keeper.
func New(d Deps) *Keeper {
	if d.TTL <= 0 {
		d.TTL = DefaultTTL
	}
	return &Keeper{d: d}
}

// Hash returns the request hash of a service command: the SHA-256 of its
// JSON. Include every input that changes the result, path IDs too (the
// scope holds only the path template).
func Hash(cmd any) ([]byte, error) {
	b, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("idempotency: hash: %w", err)
	}
	h := sha256.Sum256(b)
	return h[:], nil
}

// Do runs fn once per Idempotency-Key. fn's writes, the key and the stored
// result share one transaction (nested WithTx calls in fn join it). Without
// a key in ctx (no header, anonymous caller, worker) fn simply runs in a
// transaction. The result T must survive a JSON round trip: a replay
// returns the unmarshalled stored value and replayed=true.
func Do[T any](ctx context.Context, k *Keeper, reqHash []byte, fn func(ctx context.Context) (T, error)) (out T, replayed bool, err error) {
	key, ok := KeyFrom(ctx)
	if !ok {
		err = k.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			out, err = fn(ctx)
			return err
		})
		return out, false, err
	}
	if len(reqHash) != sha256.Size {
		return out, false, errors.New("idempotency: request hash must be a SHA-256 (use Hash)")
	}
	err = k.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		now := k.d.Clock.Now()
		rec, reserved, err := k.d.Store.Reserve(ctx, key.Scope, key.Value, reqHash, now, now.Add(lockFor), now.Add(k.d.TTL))
		if err != nil {
			return err
		}
		if !reserved {
			switch {
			case !bytes.Equal(rec.RequestHash, reqHash):
				return ErrKeyReused
			case rec.Status != StatusCompleted:
				return ErrInProgress
			}
			if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
				return fmt.Errorf("idempotency: stored response: %w", err)
			}
			replayed = true
			return nil
		}
		v, err := fn(ctx)
		if err != nil {
			return err // rollback: the key is released with the business data
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("idempotency: encode response: %w", err)
		}
		if err := k.d.Store.Complete(ctx, key.Scope, key.Value, key.Status, b); err != nil {
			return err
		}
		out = v
		return nil
	})
	if err != nil {
		var zero T
		return zero, false, err
	}
	if replayed {
		key.markReplayed()
	}
	return out, replayed, nil
}

// Cleanup job (01 §7: idempotency.cleanup, hourly at :05, low).
const (
	CleanupJobName = "idempotency.cleanup"
	cleanupCron    = "5 * * * *"
	cleanupUnique  = 55 * time.Minute
	cleanupBatch   = 1000
)

// CleanupJob deletes expired keys in batches.
func (k *Keeper) CleanupJob() queue.Job {
	return queue.Job{
		Name: CleanupJobName, Cron: cleanupCron, Queue: queue.Low, Unique: cleanupUnique,
		Handle: func(ctx context.Context) error {
			now := k.d.Clock.Now()
			total, err := db.Drain(ctx, cleanupBatch, func(ctx context.Context) (int, error) {
				return k.d.Store.DeleteExpired(ctx, now, cleanupBatch)
			})
			if err != nil {
				return err
			}
			if k.d.Log != nil {
				k.d.Log.LogAttrs(ctx, slog.LevelInfo, "idempotency cleanup done", slog.Int("deleted", total))
			}
			return nil
		},
	}
}
