package consent

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of consent (05 §2.2). No consent table
// is org-owned (02 §4: "Org kapsamı: yok"): every method is cross-org and
// addresses rows by user, purpose or ID. The GORM store (store_gorm.go)
// and the in-memory store (store_mem.go) pass the same contract test
// (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any. The ledger
// is append-only: there is no method that changes or deletes a consent.
type Store interface {
	// TermsStore is the terms of use acceptance ledger (BE-118).
	TermsStore

	// Purposes returns every purpose ordered by code.
	Purposes(ctx context.Context) ([]PurposeDef, error)
	// Purpose returns one purpose or ErrUnknownPurpose.
	Purpose(ctx context.Context, code Purpose) (PurposeDef, error)
	// LockPurpose returns the purpose locked for the rest of the transaction
	// (SELECT … FOR UPDATE), or ErrUnknownPurpose.
	LockPurpose(ctx context.Context, code Purpose) (PurposeDef, error)
	// UpdatePurpose writes description_tr and active of p.
	UpdatePurpose(ctx context.Context, p PurposeDef) error

	// InsertNotice writes a draft (published_at and retired_at are ignored).
	// A duplicate (kind, purpose, audience, locale, version) is
	// ErrNoticeVersionTaken; an unknown purpose ErrUnknownPurpose.
	InsertNotice(ctx context.Context, n Notice) error
	// Notice returns a notice or ErrNotFound.
	Notice(ctx context.Context, id uuid.UUID) (Notice, error)
	// LockNotice returns a notice locked for the transaction, or ErrNotFound.
	LockNotice(ctx context.Context, id uuid.UUID) (Notice, error)
	// LockCurrent returns the published, unretired notice of key locked
	// for the transaction; found=false when the key has none.
	LockCurrent(ctx context.Context, key NoticeKey) (n Notice, found bool, err error)
	// ListNotices returns up to limit notices matching f — drafts, current
	// and retired versions — newest first (id DESC: UUIDv7 creation
	// order), after the position after (nil: the first page; BE-129).
	ListNotices(ctx context.Context, f NoticeListFilter, after *uuid.UUID, limit int) ([]Notice, error)
	// CurrentNotices returns the published, unretired notices matching q,
	// ordered by kind, purpose, audience and locale.
	CurrentNotices(ctx context.Context, q CurrentQuery) ([]Notice, error)
	// Publish sets published_at of a draft. ErrNoticeState when the notice
	// is not a draft; ErrNoticeState too when another version of its key
	// is current (notice_versions_current_uq).
	Publish(ctx context.Context, id uuid.UUID, at time.Time) error
	// Retire sets retired_at of a published, unretired notice (the only
	// change notice_versions_guard allows); ErrNoticeState otherwise.
	Retire(ctx context.Context, id uuid.UUID, at time.Time) error

	// InsertConsent appends a consent row.
	InsertConsent(ctx context.Context, c Consent) error
	// Latest returns the newest row of (user, purpose, scope), ordered by
	// occurred_at then id; found=false when there is none. scopeID nil
	// means the unscoped row.
	Latest(ctx context.Context, userID uuid.UUID, purpose Purpose, scopeID *uuid.UUID) (c Consent, found bool, err error)
	// LockSubject serialises the unscoped chain of (user, purpose) for the
	// rest of the transaction carried by ctx (a transaction-scoped advisory
	// lock): concurrent one-click unsubscribes read the latest row one
	// after the other and withdraw once (BE-062). Outside a transaction it
	// guards nothing.
	LockSubject(ctx context.Context, userID uuid.UUID, purpose Purpose) error
	// CurrentOf returns the newest row of every (purpose, scope) of a user
	// (the consent_current view), ordered by purpose then scope.
	CurrentOf(ctx context.Context, userID uuid.UUID) ([]Consent, error)
	// GrantedAmong returns those of userIDs whose newest unscoped row of
	// purpose is a grant, in ascending ID order.
	GrantedAmong(ctx context.Context, purpose Purpose, userIDs []uuid.UUID) ([]uuid.UUID, error)
	// GrantedChains returns up to limit newest unscoped rows of purpose that
	// are grants (one per user: the consent in force), with a user ID
	// greater than after, in ascending user ID order
	// (consent.iys_status_sync, BE-103).
	GrantedChains(ctx context.Context, purpose Purpose, after uuid.UUID, limit int) ([]Consent, error)
	// ListConsents returns up to q.Limit rows of a user, newest first
	// (occurred_at DESC, id DESC), after q.After.
	ListConsents(ctx context.Context, q ListQuery) ([]Consent, error)
	// ConsentByID returns one consent row or ErrNotFound.
	ConsentByID(ctx context.Context, id uuid.UUID) (Consent, error)
	// AllOf returns every row of a user, oldest first (KVKK export).
	AllOf(ctx context.Context, userID uuid.UUID) ([]Consent, error)

	// InsertSubmission writes a pending İYS submission; a second one for
	// the same consent is a conflict (iys_submissions_consent_id_uq). The
	// store stamps created_at and updated_at (s's are ignored, BE-069).
	InsertSubmission(ctx context.Context, s Submission) error
	// SubmissionOf returns the submission of a consent or ErrNotFound.
	SubmissionOf(ctx context.Context, consentID uuid.UUID) (Submission, error)
	// LockSubmission returns a submission locked for the transaction.
	LockSubmission(ctx context.Context, id uuid.UUID) (Submission, error)
	// UpdateSubmission writes status, iys_transaction_id, attempts,
	// last_error and sent_at (updated_at is set by the trigger).
	UpdateSubmission(ctx context.Context, s Submission) error
	// DueSubmissions returns up to limit pending or failed submissions last
	// updated before the cutoff, with an ID greater than after, in ID order
	// (consent.iys_reconcile).
	DueSubmissions(ctx context.Context, before time.Time, after uuid.UUID, limit int) ([]Submission, error)

	// IYSSync returns the reconciliation state of an İYS channel;
	// found=false before the first complete run (BE-103).
	IYSSync(ctx context.Context, ch IYSChannel) (st IYSSync, found bool, err error)
	// SaveIYSSync writes channel, synced_at, checked and withdrawn of st
	// (updated_at is the database's). A row whose synced_at is newer than
	// st's is kept: a late run never moves the state back.
	SaveIYSSync(ctx context.Context, st IYSSync) error

	// IYSFeed returns the read position of an İYS change feed; found=false
	// before its first run (BE-105).
	IYSFeed(ctx context.Context, feed string) (st IYSFeedState, found bool, err error)
	// AdvanceIYSFeed writes cursor and horizon of feed (creating the row)
	// unless the stored horizon is newer: a late or concurrent run never
	// moves the feed back.
	AdvanceIYSFeed(ctx context.Context, feed, cursor string, horizon time.Time) error
	// MarkIYSSweep records that a full reconciliation of feed is due since
	// at (the feed lost changes); the latest mark is kept (creating the row).
	MarkIYSSweep(ctx context.Context, feed string, at time.Time) error
	// ClearIYSSweep ends the due reconciliation of feed when it was marked
	// at or before upTo (a reconciliation that started at upTo covers it).
	ClearIYSSweep(ctx context.Context, feed string, upTo time.Time) error

	// PurgeProof deletes up to limit consent rows (with their İYS
	// submissions) of the chains whose proof retention ended: the latest
	// row of the (user, purpose, scope) chain is a withdrawal before cutoff,
	// or the user was erased before cutoff (consent_proof, 3 years; BE-057).
	// Oldest first, so a chain cut by the limit keeps its newest rows and
	// its current state. dryRun counts every such row and deletes nothing.
	// The GORM store goes through 000121's consents_purge (consents are
	// append-only): it never passes now − the policy's period and refuses a
	// real purge of an unapproved policy (55000).
	PurgeProof(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
}

// TermsStore is the terms_acceptances part of Store (000148, BE-118): an
// append-only ledger addressed by user, kept apart from the consents of the
// main interface (05 §2.2: one embedded interface per aggregate).
type TermsStore interface {
	// InsertTermsAcceptance appends t; inserted=false (and no error) when
	// the user already accepted that notice version
	// (terms_acceptances_user_notice_uq): the earlier row stands. The store
	// stamps created_at. A notice that is not a terms of use, or whose body
	// digest is not t.TextSHA256, is a conflict (terms_acceptances_notice_fkey).
	InsertTermsAcceptance(ctx context.Context, t TermsAcceptance) (inserted bool, err error)
	// TermsAcceptancesOf returns every acceptance of a user, oldest first
	// (accepted_at, id): the status read and the KVKK export.
	TermsAcceptancesOf(ctx context.Context, userID uuid.UUID) ([]TermsAcceptance, error)
	// PurgeTerms deletes up to limit acceptances of the accounts erased
	// before cutoff, oldest first (terms_acceptance, 3 years). dryRun
	// counts every such row and deletes nothing. The GORM store goes
	// through 000148's terms_acceptances_purge (the table is append-only):
	// it never passes now − the policy's period and refuses a real purge of
	// an unapproved policy (55000). The in-memory store knows no accounts
	// and purges nothing.
	PurgeTerms(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
}

// CurrentQuery filters Store.CurrentNotices. Empty slices and nil pointers
// do not filter.
type CurrentQuery struct {
	Kind      *NoticeKind
	Purpose   *Purpose
	Audiences []Audience
	Locales   []string
}

// NoticeListFilter filters Store.ListNotices; nil pointers do not filter.
type NoticeListFilter struct {
	Kind     *NoticeKind
	Purpose  *Purpose
	Audience *Audience
	Locale   *string
	Status   *NoticeStatus
}

// ListQuery is one page of a user's consents.
type ListQuery struct {
	UserID  uuid.UUID
	Purpose *Purpose
	After   *After
	Limit   int
}
