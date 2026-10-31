package consent

import (
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
//
// Store is split by aggregate into embedded interfaces (05 §2.2, BE-156);
// a service file reaches the store only through the narrow view of the
// aggregate it works on (the accessors below), the retention sweeper
// through the purges it holds (retention.go), the KVKK provider
// (privacy.go), which covers every row of a data subject, through the
// whole Store.
type Store interface {
	purposeStore    // consent_purposes (store_purposes.go)
	noticeStore     // notice_versions (store_notices.go)
	consentStore    // consents, the ledger (store_consents.go)
	submissionStore // iys_submissions (store_submissions.go)
	iysStateStore   // the İYS status reconciliation state (store_iys_state.go)
	retentionStore  // the consent proof purge (store_retention.go)

	// TermsStore is the terms of use acceptance ledger (BE-118).
	TermsStore

	// UnsubscribeLinkStore holds the opaque unsubscribe links (BE-153).
	UnsubscribeLinkStore
}

// purposes is the consent_purposes view of the store.
func (s *service) purposes() purposeStore { return s.d.Store }

// notices is the notice_versions view of the store.
func (s *service) notices() noticeStore { return s.d.Store }

// consents is the ledger view of the store.
func (s *service) consents() consentStore { return s.d.Store }

// submissions is the iys_submissions view of the store.
func (s *service) submissions() submissionStore { return s.d.Store }

// iysState is the İYS status reconciliation view of the store.
func (s *service) iysState() iysStateStore { return s.d.Store }

// terms is the terms of use acceptance view of the store.
func (s *service) terms() TermsStore { return s.d.Store }

// unsubscribeLinks is the store view of the opaque links.
func (s *service) unsubscribeLinks() UnsubscribeLinkStore { return s.d.Store }

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
