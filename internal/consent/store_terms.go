package consent

import (
	"context"
	"time"
	"uuid"
)

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
	// before cutoff, oldest first (terms_acceptance, 10 years). dryRun
	// counts every such row and deletes nothing. The GORM store goes
	// through 000148's terms_acceptances_purge (the table is append-only):
	// it never passes now − the policy's period and refuses a real purge of
	// an unapproved policy (55000). The in-memory store knows no accounts
	// and purges nothing.
	PurgeTerms(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
}
