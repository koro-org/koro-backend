// Package kvkk is the KVKK module, part 1 (01 §4.22; BE-021): data subject
// requests (KVKK md. 11) with their 30-day legal deadline, the data export
// built from every registered privacy.Provider, and the account erasure
// orchestration.
//
// Rules the package enforces:
//
//   - Every request gets due_at = received_at + 30 days (md. 13) and stays
//     on the DPO's queue until it is completed, rejected or cancelled;
//     kvkk.dsr_deadlines (09:00) warns the DPO when ≤ 7 days are left.
//   - Identity first: an erasure request needs the re-authentication proof
//     in the same request (03 k31, §6.2; identity_verified_at = that
//     moment); other in-app requests come from an authenticated session.
//     A request entered by the DPO (post, KEP, e-mail, in person) cannot
//     be completed before the DPO records the identity check.
//   - A profiling report (profiling_access) needs two different approvers
//     (03 §6.1; authz four eyes + the 000088 CHECK).
//   - Export: every registered privacy.Provider is asked for its section;
//     the zip goes to files (purpose dsr_export) attached to the request.
//     Downloads are presigned URLs of at most 5 minutes and every download
//     is audited. The file is deleted 7 days after the request was
//     completed (kvkk.purge_exports, hourly; T-83, never deferred).
//   - Erasure (dsr.erasure_approved, worker): each provider erases in its
//     own transaction and is idempotent, so a failed run is simply
//     retried; identity anonymises last (tombstone row: financial and
//     audit rows keep their foreign keys, the hash chain is never
//     touched) and deletes every session; then every other provider runs
//     once more to sweep what the user wrote while the run was going.
//     Consent rows are never deleted: consent withdraws what was granted.
//   - Nothing personal is logged or put in an error: logs carry request
//     IDs, types, statuses and counts only; details, names and addresses
//     stay in the database.
package kvkk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

// Service is the kvkk service: the HTTP use cases of 04 §2.22. Every method
// calls authz.Can first (05 §5.3); R steps then verify the reauth proof
// (03 §6.2).
type Service interface {
	// CreateMine is createMyDataRequest (kvkk.dsr.create; erasure: R).
	CreateMine(ctx context.Context, a authz.Actor, in MyRequest) (DSR, error)
	// ListMine is listMyDataRequests (kvkk.dsr.list_own).
	ListMine(ctx context.Context, a authz.Actor, after *After, limit int) (Page, error)
	// GetMine is getMyDataRequest (kvkk.dsr.read_own).
	GetMine(ctx context.Context, a authz.Actor, id uuid.UUID) (DSR, error)
	// CancelMine is cancelMyDataRequest (kvkk.dsr.cancel_own).
	CancelMine(ctx context.Context, a authz.Actor, id uuid.UUID) (DSR, error)
	// DownloadMine is downloadMyDataExport (kvkk.dsr.export_download).
	DownloadMine(ctx context.Context, a authz.Actor, id uuid.UUID) (Download, error)

	// AdminList is adminListDataRequests (kvkk.dsr.list; platform_admin
	// sees only awaiting_second_approval, 03 k32).
	AdminList(ctx context.Context, a authz.Actor, f AdminFilter, after *After, limit int) (Page, error)
	// AdminGet is adminGetDataRequest (kvkk.dsr.read).
	AdminGet(ctx context.Context, a authz.Actor, id uuid.UUID) (DSR, error)
	// AdminCreate is adminCreateDataRequest (kvkk.dsr.create_admin).
	AdminCreate(ctx context.Context, a authz.Actor, in AdminRequest) (DSR, error)
	// AdminUpdate is adminUpdateDataRequest (kvkk.dsr.update; If-Match).
	AdminUpdate(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p Patch) (DSR, error)
	// Approve is adminApproveDataRequest (kvkk.dsr.approve, four eyes).
	Approve(ctx context.Context, a authz.Actor, id uuid.UUID, stage Stage) (DSR, error)
	// Complete is adminCompleteDataRequest (kvkk.dsr.complete, R).
	Complete(ctx context.Context, a authz.Actor, id uuid.UUID, summary string, proof ReauthProof) (DSR, error)
	// Reject is adminRejectDataRequest (kvkk.dsr.reject).
	Reject(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (DSR, error)
	// AdminDownload is adminDownloadDataExport (kvkk.dsr.export_download).
	AdminDownload(ctx context.Context, a authz.Actor, id uuid.UUID) (Download, error)
}

// Deps are the dependencies of the kvkk module. Everything but Talent and
// DPOEmail is required (New returns an error otherwise, 05 §3).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Outbox      outbox.Writer // dsr.* events (mechanism O)
	Audit       AuditLogger
	Idempotency *idempotency.Keeper // optional Idempotency-Key of the POST operations
	Clock       clock.Clock
	Log         *slog.Logger

	// Reauth verifies R proofs (identity). nil → every R step fails closed.
	Reauth Reauthenticator
	// Users reads identity (existence of a user, the worker's address).
	Users Users
	// Files stores, attaches and serves the export file; RemoveFile
	// deletes it (see FileRemover).
	Files      Files
	RemoveFile FileRemover
	// Mailer sends the DPO and data subject e-mails (worker).
	Mailer Mailer
	// Providers returns the registered privacy providers at call time
	// (container.privacy, 01 §3.3; kvkk's own provider included).
	Providers func() []privacy.Provider
	// Talent is the auditor report of profiling_access (hooks.talentAuditor).
	// nil → the export states that no profiling report exists.
	Talent TalentAuditor

	// DPOEmail is KVKK_DPO_EMAIL; empty → DPO mails are logged and skipped.
	DPOEmail string
	// ExportRetention is KVKK_EXPORT_RETENTION (≤ 7 days, T-83); 0 → 7 days.
	ExportRetention time.Duration
}

func (d *Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Outbox", d.Outbox != nil}, {"Audit", d.Audit != nil},
		{"Idempotency", d.Idempotency != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
		{"Users", d.Users != nil}, {"Files", d.Files != nil}, {"RemoveFile", d.RemoveFile != nil},
		{"Mailer", d.Mailer != nil}, {"Providers", d.Providers != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("kvkk.Deps.%s is missing", c.name))
		}
	}
	if d.ExportRetention == 0 {
		d.ExportRetention = MaxExportRetention
	}
	if d.ExportRetention < 0 || d.ExportRetention > MaxExportRetention {
		errs = append(errs, fmt.Errorf("kvkk: export retention %s exceeds %s (T-83)", d.ExportRetention, MaxExportRetention))
	}
	return errors.Join(errs...)
}

// MaxExportRetention is the longest an export file lives (T-83: 7 days).
const MaxExportRetention = 7 * 24 * time.Hour

// DownloadTTL is the lifetime of an export download URL (04 §1.11: ≤ 5
// minutes).
const DownloadTTL = storage.MaxGetTTL

// Module is what kvkk hands to the composition root.
type Module struct {
	Service Service
	// Privacy is kvkk's own provider: the subject's requests are exported;
	// on erasure their export files are deleted and the rows kept (proof).
	Privacy privacy.Provider

	svc *service
}

// New builds the kvkk module.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Privacy: privacyProvider{s: s}, svc: s}, nil
}

// Subscribe registers the outbox subscriptions of kvkk (01 §6.2) on the
// critical queue (01 §8: KVKK): dsr.submitted → DPO mail,
// dsr.erasure_approved → erasure, dsr.completed → export build + subject
// mail.
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(TopicSubmitted, NotifyDPOTaskName, m.svc.handleSubmitted, outbox.Queue(queue.Critical))
	r.Subscribe(TopicErasureApproved, EraseAccountTaskName, m.svc.handleErasureApproved,
		outbox.Queue(queue.Critical), outbox.Timeout(eraseTimeout))
	r.Subscribe(TopicCompleted, FinishRequestTaskName, m.svc.handleCompleted,
		outbox.Queue(queue.Critical), outbox.Timeout(exportTimeout))
}

// Jobs returns the scheduled jobs of kvkk (01 §7): kvkk.dsr_deadlines and
// kvkk.purge_exports.
func (m Module) Jobs() []queue.Job {
	return []queue.Job{m.svc.deadlinesJob(), m.svc.purgeExportsJob()}
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
