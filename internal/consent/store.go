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
	// CurrentOf returns the newest row of every (purpose, scope) of a user
	// (the consent_current view), ordered by purpose then scope.
	CurrentOf(ctx context.Context, userID uuid.UUID) ([]Consent, error)
	// GrantedAmong returns those of userIDs whose newest unscoped row of
	// purpose is a grant, in ascending ID order.
	GrantedAmong(ctx context.Context, purpose Purpose, userIDs []uuid.UUID) ([]uuid.UUID, error)
	// ListConsents returns up to q.Limit rows of a user, newest first
	// (occurred_at DESC, id DESC), after q.After.
	ListConsents(ctx context.Context, q ListQuery) ([]Consent, error)
	// ConsentByID returns one consent row or ErrNotFound.
	ConsentByID(ctx context.Context, id uuid.UUID) (Consent, error)
	// AllOf returns every row of a user, oldest first (KVKK export).
	AllOf(ctx context.Context, userID uuid.UUID) ([]Consent, error)

	// InsertSubmission writes a pending İYS submission; a second one for
	// the same consent is a conflict (iys_submissions_consent_id_uq).
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
}

// CurrentQuery filters Store.CurrentNotices. Empty slices and nil pointers
// do not filter.
type CurrentQuery struct {
	Kind      *NoticeKind
	Purpose   *Purpose
	Audiences []Audience
	Locales   []string
}

// ListQuery is one page of a user's consents.
type ListQuery struct {
	UserID  uuid.UUID
	Purpose *Purpose
	After   *After
	Limit   int
}
