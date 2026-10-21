// Package audit is Koro's append-only audit log (01 §4.1, 02 §2): the
// Logger every module calls in its own transaction, the hash chain, the
// signed daily roots and their verification, and the four audit endpoints
// (04 §2.21).
//
// How the log stays immutable:
//
//   - The application role (app_rw) may only INSERT rows (through
//     audit_writer) and SELECT them; it can neither update nor delete
//     (02 §0.3, 000006). audit_events_guard rejects any change of a chained
//     row and any delete outside audit_purge_until().
//   - The chain job (audit.chain, every 10 s) is the only writer of the
//     chain columns. It runs SET LOCAL ROLE audit_chainer, which may update
//     chain_seq, prev_hash, row_hash and chained_at and nothing else, and
//     serialises itself with a transaction advisory lock. Rows are chained
//     in seq order: chain_seq = previous + 1, prev_hash = previous row_hash
//     (32 zero bytes for the first row), row_hash = SHA-256(prev_hash ‖
//     canonical JSON of the row without the chain columns) (RowHash).
//   - audit.daily_root (00:15 Europe/Istanbul) signs the chain head as it
//     stood at the end of each finished day (Ed25519; OpenBao Transit with
//     BE-055), stores it in audit_daily_roots and writes a copy to a
//     separate write-once store (RootArchive; object lock is OPS work).
//   - audit.verify_chain (05:00) and POST /admin/audit/verify recompute the
//     chain from the last root before the range: a deleted row is a gap in
//     chain_seq, an edited or inserted row breaks prev_hash/row_hash, a
//     rewritten tail no longer matches the signed root. A broken chain
//     raises the alarm hook (Deps.Alarm).
//
// What never enters a row: passwords, tokens, keys or any special-category
// personal data (KVKK md. 6) as a value. Log rejects such metadata
// (ErrSecretMetadata); IP, User-Agent and request ID come from the context.
package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// Logger is the cross-module contract of audit (01 §4.1; every module →
// audit.Logger.Log is a synchronous write of 01 §6.3). Consumers declare it
// in their ports.go with "// sync-write: audit.Logger.Log".
type Logger interface {
	// Log appends e. When ctx carries a transaction (db.TxManager.WithTx)
	// the row is written in it, so it commits or rolls back with the
	// business change. IP, User-Agent and request ID are taken from ctx.
	// Errors: ErrInvalidEvent, ErrSecretMetadata (programmer errors, 500)
	// or a store error.
	Log(ctx context.Context, e Event) error
}

// Service is the audit service: the Logger plus the HTTP use cases. Every
// method that takes an actor calls authz.Can first (05 §5.3).
type Service interface {
	Logger
	// ListOwn lists the security events of the actor's own account
	// (audit.event.list_own), sign-in attempts against it summarised
	// (SecurityEvent).
	ListOwn(ctx context.Context, a authz.Actor, q OwnQuery) (Page[SecurityEvent], error)
	// ListOrg lists the events of one org (audit.event.list_org).
	ListOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID, q OrgQuery) (Page[View], error)
	// AdminList searches every event (audit.event.list); the search itself
	// is audited in the same transaction.
	AdminList(ctx context.Context, a authz.Actor, q AdminQuery) (Page[View], error)
	// Verify checks the chain and the daily roots of a day range
	// (audit.chain.verify); the check is audited.
	Verify(ctx context.Context, a authz.Actor, from, to civil.Date) (VerifyResult, error)
}

// Deps are the dependencies of the audit module. Store, Tx, Clock and Log
// are required (New panics otherwise, 05 §3); the rest has a documented
// default.
type Deps struct {
	Store Store
	Tx    db.TxManager
	Clock clock.Clock
	Log   *slog.Logger

	// Signer signs the daily roots and verifies their signatures. nil →
	// audit.daily_root fails with ErrSignerNotConfigured (never an
	// in-memory key, 05 §12.2) and a range with roots cannot be verified.
	Signer RootSigner
	// Archive is the separate write-once store of the daily roots. nil →
	// audit.daily_root fails with ErrArchiveNotConfigured.
	Archive RootArchive
	// Alarm is raised for a broken chain, a chain that lags behind or a
	// failed daily root (GlitchTip + DPO, 01 §7). nil → an ERROR log line
	// with alarm=true.
	Alarm func(ctx context.Context, a Alarm)
	// Idempotency stores the result of POST /admin/audit/verify under its
	// optional Idempotency-Key (06 H-5). nil → no replay.
	Idempotency *idempotency.Keeper
	// ActorNames returns display names for actor IDs (identity, BE-014).
	// nil → actor.display is omitted.
	ActorNames ActorNames
}

func (d *Deps) mustValid() {
	var missing []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil},
		{"Tx", d.Tx != nil},
		{"Clock", d.Clock != nil},
		{"Log", d.Log != nil},
	} {
		if !c.ok {
			missing = append(missing, fmt.Errorf("audit.Deps.%s is missing", c.name))
		}
	}
	if err := errors.Join(missing...); err != nil {
		panic(err)
	}
}

// Module is what audit hands to the composition root.
type Module struct {
	Service Service
	// Jobs are audit.chain, audit.daily_root and audit.verify_chain (01 §7).
	Jobs []queue.Job
	// Sweeper ends the retention of the log and of its daily roots
	// (02 §24.2 audit_events, audit_roots; BE-057), run by
	// kvkk.retention_sweep.
	Sweeper privacy.RetentionSweeper
}

// New builds the audit module.
func New(d Deps) Module {
	d.mustValid()
	s := &service{d: d}
	return Module{Service: s, Jobs: s.jobs(), Sweeper: sweeper{s: s}}
}

type service struct {
	d Deps
	// chainMu serialises chain runs of one process; the advisory lock of
	// Store.BeginChain serialises processes.
	chainMu sync.Mutex
}

// now is the service clock in the database's precision (microseconds), so
// the in-memory store hashes exactly what PostgreSQL stores.
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }

func (s *service) alarm(ctx context.Context, a Alarm) {
	if s.d.Alarm != nil {
		s.d.Alarm(ctx, a)
		return
	}
	attrs := []slog.Attr{slog.Bool("alarm", true), slog.String("kind", string(a.Kind))}
	if a.FirstBadChainSeq != nil {
		attrs = append(attrs, slog.Int64("first_bad_chain_seq", *a.FirstBadChainSeq))
	}
	if a.Detail != "" {
		attrs = append(attrs, slog.String("detail", a.Detail))
	}
	s.d.Log.LogAttrs(ctx, slog.LevelError, "audit alarm", attrs...)
}

// AlarmKind classifies an Alarm.
type AlarmKind string

// Alarm kinds.
const (
	AlarmChainBroken AlarmKind = "chain_broken" // tampering: gap, hash or root mismatch, bad signature
	AlarmChainLag    AlarmKind = "chain_lag"    // rows stay unchained (the chain job is not running)
	AlarmRootFailed  AlarmKind = "root_failed"  // a daily root could not be signed or archived
)

// Alarm is raised by the jobs and by a failed verification.
type Alarm struct {
	Kind             AlarmKind
	FirstBadChainSeq *int64
	Detail           string // never personal data
}
