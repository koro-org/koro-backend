package consent

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"time"
	"uuid"
)

// Purpose is consent_purposes.code (02 §4.1): what a user consents to.
type Purpose string

// The purposes seeded by migration 000011 (02 §4.1).
const (
	PurposeCommercialEmail  Purpose = "commercial_email"
	PurposeCommercialSMS    Purpose = "commercial_sms"
	PurposeCommercialPush   Purpose = "commercial_push"
	PurposeDiscoveryProfile Purpose = "discovery_profile"
	PurposeContactInfoShare Purpose = "contact_info_share"
	PurposeEventSponsor     Purpose = "event_sponsor_share"
	PurposeActivityRanking  Purpose = "activity_ranking"
)

// LegalBasis is consent_purposes.legal_basis. Only explicit_consent purposes
// are recorded in the ledger: a purpose that rests on the contract (or any
// other basis) is not consented to, so consent can never be bundled with
// the terms of service (Kurul 2026/347).
type LegalBasis string

// Legal bases.
const (
	BasisExplicitConsent    LegalBasis = "explicit_consent"
	BasisContract           LegalBasis = "contract"
	BasisLegalObligation    LegalBasis = "legal_obligation"
	BasisLegitimateInterest LegalBasis = "legitimate_interest"
)

// ScopeType is consent_purposes.scope_type and consents.scope_type: whether a
// consent is given once per user ("none") or per event registration.
type ScopeType string

// Scope types.
const (
	ScopeNone         ScopeType = "none"
	ScopeRegistration ScopeType = "registration"
)

// IYSChannel is the İYS (commercial message registry) channel of a purpose.
type IYSChannel string

// İYS channels (02 §4.1, §4.4).
const (
	IYSEmail IYSChannel = "email"
	IYSSMS   IYSChannel = "sms"
	IYSCall  IYSChannel = "call"
)

// PurposeDef is one consent_purposes row (02 §4.1). Rows are never deleted:
// Active=false retires a purpose.
type PurposeDef struct {
	Code          Purpose     `gorm:"column:code;primaryKey"`
	DescriptionTR string      `gorm:"column:description_tr"`
	LegalBasis    LegalBasis  `gorm:"column:legal_basis"`
	RequiresIYS   bool        `gorm:"column:requires_iys"`
	IYSChannel    *IYSChannel `gorm:"column:iys_channel"`
	ScopeType     ScopeType   `gorm:"column:scope_type"`
	Active        bool        `gorm:"column:active"`
	CreatedAt     time.Time   `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (PurposeDef) TableName() string { return "consent_purposes" }

// minorRestricted is 03 k12: a minor may not grant these purposes (the
// commercial_* channels, Level 2 discovery, contact sharing and sponsor
// sharing). Withdrawing is always allowed.
func (p PurposeDef) minorRestricted() bool {
	switch p.Code {
	case PurposeDiscoveryProfile, PurposeContactInfoShare, PurposeEventSponsor:
		return true
	}
	return strings.HasPrefix(string(p.Code), "commercial_")
}

// NoticeKind is notice_versions.kind (02 §4.2). A privacy notice
// (aydınlatma) is shown, never consented to; a consent text belongs to
// exactly one purpose; terms of use are never consent proof.
type NoticeKind string

// Notice kinds.
const (
	KindPrivacyNotice NoticeKind = "privacy_notice"
	KindConsentText   NoticeKind = "consent_text"
	KindTermsOfUse    NoticeKind = "terms_of_use"
	KindCookieNotice  NoticeKind = "cookie_notice"
)

// Audience is notice_versions.audience.
type Audience string

// Audiences. AudienceAll is the fallback of every other audience.
const (
	AudienceAll        Audience = "all"
	AudienceStudent    Audience = "student"
	AudienceCompany    Audience = "company"
	AudienceClub       Audience = "club"
	AudienceUniversity Audience = "university"
)

// Locales of notice_versions.locale. LocaleTR is the fallback.
const (
	LocaleTR = "tr-TR"
	LocaleEN = "en-US"
)

// NoticeStatus is derived from published_at and retired_at.
type NoticeStatus string

// Notice statuses.
const (
	StatusDraft     NoticeStatus = "draft"     // editable, invisible to the public
	StatusPublished NoticeStatus = "published" // the current version of its key
	StatusRetired   NoticeStatus = "retired"   // replaced or withdrawn; still readable (proof)
)

// Notice is one notice_versions row (02 §4.2). A published row never
// changes again except for retired_at, once (notice_versions_guard).
type Notice struct {
	ID          uuid.UUID  `gorm:"column:id;primaryKey"`
	Kind        NoticeKind `gorm:"column:kind"`
	PurposeCode *Purpose   `gorm:"column:purpose_code"`
	Audience    Audience   `gorm:"column:audience"`
	Locale      string     `gorm:"column:locale"`
	Version     string     `gorm:"column:version"`
	Title       string     `gorm:"column:title"`
	BodyMD      string     `gorm:"column:body_md"`
	BodySHA256  []byte     `gorm:"column:body_sha256"`
	PublishedAt *time.Time `gorm:"column:published_at"`
	RetiredAt   *time.Time `gorm:"column:retired_at"`
	CreatedBy   *uuid.UUID `gorm:"column:created_by"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (Notice) TableName() string { return "notice_versions" }

// Status returns the notice's lifecycle state.
func (n Notice) Status() NoticeStatus {
	switch {
	case n.RetiredAt != nil:
		return StatusRetired
	case n.PublishedAt != nil:
		return StatusPublished
	}
	return StatusDraft
}

// Key identifies the slot a published notice occupies (one current version
// per key; notice_versions_current_uq).
func (n Notice) Key() NoticeKey {
	return NoticeKey{Kind: n.Kind, Purpose: n.PurposeCode, Audience: n.Audience, Locale: n.Locale}
}

func (n Notice) clone() Notice {
	c := n
	c.BodySHA256 = bytes.Clone(n.BodySHA256)
	if n.PurposeCode != nil {
		p := *n.PurposeCode
		c.PurposeCode = &p
	}
	return c
}

// NoticeKey is (kind, purpose, audience, locale): the key of
// notice_versions_current_uq.
type NoticeKey struct {
	Kind     NoticeKind
	Purpose  *Purpose // set for consent texts only
	Audience Audience
	Locale   string
}

// same reports whether k and o name the same slot.
func (k NoticeKey) same(o NoticeKey) bool {
	return k.Kind == o.Kind && k.purpose() == o.purpose() && k.Audience == o.Audience && k.Locale == o.Locale
}

func (k NoticeKey) purpose() Purpose {
	if k.Purpose == nil {
		return ""
	}
	return *k.Purpose
}

// BodyDigest is body_sha256: the SHA-256 of the exact UTF-8 bytes of a
// notice body. Clients send it back as text_sha256 to prove which text was
// shown.
func BodyDigest(body string) []byte {
	s := sha256.Sum256([]byte(body))
	return s[:]
}

// Action is consents.action. The ledger only grows: a withdrawal is a new
// row (02 §4.3).
type Action string

// Consent actions.
const (
	Granted   Action = "granted"
	Withdrawn Action = "withdrawn"
)

// Channel is consents.channel: where the consent act happened.
type Channel string

// Channels. ChannelEmailLink is withdrawal-only (one-click unsubscribe): an
// explicit consent is an affirmative act on a page that shows the consent
// text, never a click in a message (and never part of a verification/OTP
// step, Kurul 2025/1072).
const (
	ChannelWeb       Channel = "web"
	ChannelMobile    Channel = "mobile"
	ChannelEmailLink Channel = "email_link"
	ChannelAdmin     Channel = "admin"
	ChannelImport    Channel = "import"
)

// Consent is one consents row (02 §4.3, append-only).
type Consent struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	PurposeCode     Purpose
	Action          Action
	NoticeVersionID *uuid.UUID
	TextSHA256      []byte
	ScopeType       ScopeType
	ScopeID         *uuid.UUID
	Channel         Channel
	IP              string // canonical text, "" when unknown
	UserAgent       string
	OccurredAt      time.Time
	CreatedAt       time.Time
}

func (c Consent) clone() Consent {
	out := c
	out.TextSHA256 = bytes.Clone(c.TextSHA256)
	return out
}

// SubmissionStatus is iys_submissions.status.
//
//	pending ──► sent        İYS accepted the record (terminal)
//	   │   ╲
//	   │    ► skipped       no real İYS client configured (terminal; dev)
//	   ▼
//	failed ──► sent | failed | skipped   (retried by the task and consent.iys_reconcile)
type SubmissionStatus string

// Submission statuses.
const (
	SubmissionPending SubmissionStatus = "pending"
	SubmissionSent    SubmissionStatus = "sent"
	SubmissionFailed  SubmissionStatus = "failed"
	SubmissionSkipped SubmissionStatus = "skipped"
)

// open reports whether the submission still has to reach İYS.
func (s SubmissionStatus) open() bool { return s == SubmissionPending || s == SubmissionFailed }

// Submission is one iys_submissions row (02 §4.4): the İYS delivery of one
// consent row of a requires_iys purpose. CreatedAt and UpdatedAt are read
// only: the column defaults and iys_submissions_touch_updated_at stamp them
// with the database clock alone (02 §0.1 [ts], BE-069).
type Submission struct {
	ID               uuid.UUID        `gorm:"column:id;primaryKey"`
	ConsentID        uuid.UUID        `gorm:"column:consent_id"`
	Channel          IYSChannel       `gorm:"column:channel"`
	Status           SubmissionStatus `gorm:"column:status"`
	IYSTransactionID *string          `gorm:"column:iys_transaction_id"`
	Attempts         int              `gorm:"column:attempts"`
	LastError        *string          `gorm:"column:last_error"`
	SentAt           *time.Time       `gorm:"column:sent_at"`
	CreatedAt        time.Time        `gorm:"column:created_at;->"`
	UpdatedAt        time.Time        `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Submission) TableName() string { return "iys_submissions" }

// After is the keyset position of a consent list (occurred_at DESC, id DESC).
type After struct {
	At time.Time
	ID uuid.UUID
}

// Page is one page of consents, newest first; Next is nil on the last page.
type Page struct {
	Items []Consent
	Next  *After
}
