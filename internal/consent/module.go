// Package consent is the consent module (01 §4.3; BE-012): versioned notice
// texts, the append-only consent ledger, the current-state queries other
// modules gate on, and the İYS delivery of commercial consents.
//
// Rules the package enforces (KVKK, 6563, Kurul 2026/347, 2025/1072):
//
//   - A privacy notice (aydınlatma) is shown, never consented to; a consent
//     text (açık rıza) is a separate notice of exactly one purpose. A grant
//     must name the current consent text of its purpose and the SHA-256 of
//     the text the user saw; terms of use are never consent proof.
//   - Only explicit_consent purposes are recorded: consent cannot be bundled
//     with the service contract. One record = one purpose; there is no
//     "accept all".
//   - Grants happen on a page that shows the text (web, mobile; admin and
//     import for proof migration). Email links only withdraw (one-click
//     unsubscribe), so a marketing consent can never ride on a
//     verification/OTP message. Callers (identity registration, events
//     forms) record each purpose separately, unchecked by default.
//   - The ledger only grows: a withdrawal is a new row, the newest row of
//     (user, purpose, scope) is the current state. A published notice never
//     changes (notice_versions_guard); a new version is a new row and
//     publishing it retires the previous version of the same key.
//   - Every consent act writes a consent.recorded outbox event in the
//     recording transaction (ADR-0009, mechanism O; every purpose since
//     BE-104, consumers filter by purpose). Commercial purposes
//     (requires_iys) also get an iys_submissions row; the
//     consent.submit_iys task delivers it (a no-op for other purposes) and
//     consent.iys_reconcile (05:30) retries what is still open.
//   - Every commercial e-mail carries a one-click unsubscribe link
//     (unsubscribe.go, BE-062): a signed, expiring token bound to the user
//     and the purpose; following it withdraws through the email_link
//     channel, idempotently, without revealing the subject or its state.
//   - A refusal given in İYS itself reaches the ledger through
//     consent.iys_status_sync (iys_status.go, BE-103, BE-105) as a
//     withdrawal of channel iys; optionally (CONSENT_IYS_SYNC_MAX_AGE)
//     commercial messages are held while that reconciliation is late.
//   - İYS is reached only through the small interfaces of iys.go
//     (IYSClient, IYSFeedReader, IYSStateReader), over any adapter of
//     internal/platform/iys.
package consent

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
)

// Recorder appends consents in the caller's transaction (01 §6.3:
// identity registration, students privacy settings and events form
// consent fields → consent.Recorder.Record). Consumers declare it in their
// ports.go with "// sync-write: consent.Recorder.Record".
type Recorder interface {
	// Record validates and appends one consent row with its
	// consent.recorded outbox event and, for a requires_iys purpose, the
	// İYS submission, all in the transaction carried by ctx (a rollback of
	// the caller removes everything). It returns the consent ID.
	Record(ctx context.Context, in RecordInput) (uuid.UUID, error)
}

// Checker answers whether a user's consent is in force (01 §4.3). The
// newest row of (user, purpose, scope) decides; an inactive purpose is never
// in force.
type Checker interface {
	// HasActive reports whether userID's current consent for purpose (and
	// scopeID, for per-registration purposes; nil otherwise) is a grant.
	HasActive(ctx context.Context, userID uuid.UUID, purpose Purpose, scopeID *uuid.UUID) (bool, error)
	// FilterActive returns those of userIDs with an unscoped grant in force
	// for purpose, without duplicates, in ascending ID order (outreach
	// dispatch, notify's commercial message port; BE-015).
	FilterActive(ctx context.Context, userIDs []uuid.UUID, purpose Purpose) ([]uuid.UUID, error)
}

// Notices returns the texts to show (01 §4.3).
type Notices interface {
	// Current returns the published version for key, falling back to
	// audience "all" and then to locale tr-TR; ErrNoNotice when none.
	Current(ctx context.Context, key NoticeKey) (Notice, error)
}

// Service is the consent service: the cross-module contracts plus the HTTP
// use cases (04 §2.5). Every method that takes an actor calls authz.Can
// first (05 §5.3); R operations then verify the reauth proof (03 §6.2).
type Service interface {
	Recorder
	Checker
	Notices

	// ListCurrentNotices is listCurrentNotices (consent.notice.read).
	ListCurrentNotices(ctx context.Context, a authz.Actor, q NoticeFilter) ([]Notice, error)
	// GetNotice is getNoticeVersion: published and retired versions for
	// everyone, drafts for dpo only.
	GetNotice(ctx context.Context, a authz.Actor, id uuid.UUID) (Notice, error)
	// RecordMine is recordMyConsent (consent.record.create).
	RecordMine(ctx context.Context, a authz.Actor, in MyConsent) (Consent, error)
	// ListMine is listMyConsents (consent.record.list_own).
	ListMine(ctx context.Context, a authz.Actor, q PageQuery) (MyConsents, error)
	// AdminListUser is adminListUserConsents (consent.record.list_admin;
	// support only with a break-glass reason). The read is audited.
	AdminListUser(ctx context.Context, a authz.Actor, userID uuid.UUID, access Access, q PageQuery) (Page, error)
	// CreateNotice is adminCreateNotice: a new draft (consent.notice.manage, R).
	CreateNotice(ctx context.Context, a authz.Actor, in NoticeDraft, proof ReauthProof) (Notice, error)
	// PublishNotice publishes a draft and retires the current version of
	// its key in one transaction (consent.notice.manage, R).
	PublishNotice(ctx context.Context, a authz.Actor, id uuid.UUID, proof ReauthProof) (Notice, error)
	// RetireNotice retires a published version (consent.notice.manage, R).
	RetireNotice(ctx context.Context, a authz.Actor, id uuid.UUID, proof ReauthProof) (Notice, error)
	// ListPurposes is adminListConsentPurposes (consent.purpose.manage).
	ListPurposes(ctx context.Context, a authz.Actor) ([]PurposeDef, error)
	// UpdatePurpose is adminUpdateConsentPurpose (consent.purpose.manage, R).
	UpdatePurpose(ctx context.Context, a authz.Actor, code Purpose, p PurposePatch, proof ReauthProof) (PurposeDef, error)

	// UnsubscribeLinks mints the one-click unsubscribe links (BE-062).
	UnsubscribeLinks
	// GetUnsubscribe is getUnsubscribeLink: the purpose a valid link
	// withdraws, nothing else. The token is the credential (the subject's,
	// consent.record.create); the HTTP caller is not the actor.
	GetUnsubscribe(ctx context.Context, token string) (UnsubscribeLink, error)
	// Unsubscribe is confirmUnsubscribe (RFC 8058 one-click): withdraws the
	// link's consent if it is in force, idempotently, with the İYS RET.
	Unsubscribe(ctx context.Context, token string) error
}

// Deps are the dependencies of the consent module. Everything but IYS and
// Reauth is required (New returns an error otherwise, 05 §3).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Outbox      outbox.Writer // consent.recorded (mechanism O)
	Audit       AuditLogger
	Idempotency *idempotency.Keeper // optional Idempotency-Key of the POST operations
	Clock       clock.Clock
	Log         *slog.Logger

	// Reauth verifies the proof of R operations (identity, late bound).
	// nil → every R operation fails closed with ErrReauthUnavailable.
	Reauth Reauthenticator
	// IYS delivers commercial consents (NewIYSClient over an İYS adapter,
	// or FakeIYS). nil → NoopIYS (submissions are skipped) unless
	// IYSRequired.
	IYS IYSClient
	// IYSRequired is CONSENT_IYS_REQUIRED: refuse to start without a real
	// İYS client (live operation, H-14).
	IYSRequired bool
	// IYSFeed and IYSStates read İYS for consent.iys_status_sync (BE-103,
	// BE-105): the adapter's change feed and its single-state reads. nil →
	// IYS when it implements the reader (FakeIYS); wire passes the
	// adapter's own. Without either there is no reconciliation and the job
	// is not scheduled.
	IYSFeed   IYSFeedReader
	IYSStates IYSStateReader
	// IYSSyncMaxAge is CONSENT_IYS_SYNC_MAX_AGE: > 0 holds commercial
	// messages of an İYS channel (FilterActive fails with ErrIYSSyncStale)
	// while that channel's last complete reconciliation is older or never
	// ran. It needs an İYS reader (IYSFeed or IYSStates). 0 → no hold.
	IYSSyncMaxAge time.Duration
	// Unsubscribe configures the one-click unsubscribe links of commercial
	// e-mail (BE-062). Zero → no link is minted (commercial e-mail is
	// refused upstream) and every link is refused.
	Unsubscribe UnsubscribeConfig
}

func (d *Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Outbox", d.Outbox != nil}, {"Audit", d.Audit != nil},
		{"Idempotency", d.Idempotency != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("consent.Deps.%s is missing", c.name))
		}
	}
	if d.IYS == nil {
		d.IYS = NoopIYS{}
	}
	if d.IYSRequired && !d.IYS.Real() {
		errs = append(errs, fmt.Errorf("consent: CONSENT_IYS_REQUIRED=true needs a real İYS client, got %q (BE-055)", d.IYS.Name()))
	}
	if d.IYSFeed == nil {
		d.IYSFeed, _ = d.IYS.(IYSFeedReader)
	}
	if d.IYSStates == nil {
		d.IYSStates, _ = d.IYS.(IYSStateReader)
	}
	switch {
	case d.IYSSyncMaxAge < 0:
		errs = append(errs, errors.New("consent.Deps.IYSSyncMaxAge is negative"))
	case d.IYSSyncMaxAge > 0 && d.IYSFeed == nil && d.IYSStates == nil:
		errs = append(errs, fmt.Errorf("consent: CONSENT_IYS_SYNC_MAX_AGE needs an İYS client that reads consent states; "+
			"%q does not (BE-103, BE-105)", d.IYS.Name()))
	}
	if err := d.Unsubscribe.validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Module is what consent hands to the composition root.
type Module struct {
	Service Service
	// Privacy is the KVKK provider: consents are exported and kept as proof.
	Privacy privacy.Provider
	// Sweeper ends the retention of the consent proof (consent_proof,
	// 02 §24.2; BE-057).
	Sweeper privacy.RetentionSweeper
	// Links mints the unsubscribe links of commercial e-mail; nil when
	// Deps.Unsubscribe has no base URL (commercial e-mail is unavailable).
	Links UnsubscribeLinks

	svc *service
}

// New builds the consent module. It returns an error for invalid
// dependencies (a missing one is a wiring bug, CONSENT_IYS_REQUIRED without
// a real client a configuration error; both stop the process at start).
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	s.d.IYS = watchedIYS{IYSClient: d.IYS, s: s}
	if d.IYS.Real() && d.IYSFeed == nil && d.IYSStates == nil {
		d.Log.Warn("iys status reconciliation unavailable: refusals given in İYS do not reach the ledger (T-312)",
			slog.String("client", d.IYS.Name()))
	}
	m := Module{Service: s, Privacy: privacyProvider{s: s}, Sweeper: sweeper{s: s}, svc: s}
	if d.Unsubscribe.enabled() {
		m.Links = s
	}
	return m, nil
}

// Subscribe registers the outbox subscription of consent (01 §6.2):
// consent.recorded → consent.submit_iys on the critical queue (01 §8).
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(TopicRecorded, SubmitIYSTaskName, m.svc.handleRecorded, outbox.Queue(queue.Critical))
}

// Jobs returns the scheduled jobs of consent (01 §7): consent.iys_reconcile
// and, with an İYS status reader, consent.iys_status_sync (BE-103).
func (m Module) Jobs() []queue.Job {
	jobs := []queue.Job{m.svc.reconcileJob()}
	if m.svc.d.IYSFeed != nil || m.svc.d.IYSStates != nil {
		jobs = append(jobs, m.svc.statusSyncJob())
	}
	return jobs
}

type service struct {
	d      Deps
	alarms iysAlarms
}

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds), so
// the in-memory store keeps exactly what PostgreSQL stores.
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
