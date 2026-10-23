package audit

import (
	"encoding/json"
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// DailyRoot is one audit_daily_roots row (02 §2.2): the chain head as it
// stood at the end of Day (Europe/Istanbul), signed.
type DailyRoot struct {
	Day          civil.Date
	LastChainSeq int64
	RootHash     []byte
	Signature    []byte
	KeyID        string
	ExternalRef  string // object key in the separate store
	CreatedAt    time.Time
}

// Link is a position in the chain: a chain_seq and its row_hash. The zero
// Link (Seq 0) is the empty chain, whose hash is 32 zero bytes.
type Link struct {
	Seq  int64
	Hash []byte
}

// After is the position of a cursor: rows strictly older than (At, ID) in
// (occurred_at DESC, id DESC) order.
type After struct {
	At time.Time
	ID uuid.UUID
}

// Filter selects events of a list. Zero fields do not filter. From is
// inclusive, To exclusive.
type Filter struct {
	From, To     time.Time
	ActorUserID  *uuid.UUID
	OrgID        *uuid.UUID // adminListAuditEvents only; listOrg scopes by its org
	ResourceType string
	ResourceID   *uuid.UUID
	Action       string
	Outcome      Outcome
	BreakGlass   *bool
	// HideSensitive drops rows with metadata.sensitive = true (list_org
	// for everyone but the club's club_admin, 03 §5.21).
	HideSensitive bool
}

// Page is one page of a list.
type Page[T any] struct {
	Items []T
	// Next is the position after the last item when another page exists.
	Next *After
}

// PageQuery is the paging part of every list query.
type PageQuery struct {
	Limit int    // 1–100 (page.ClampLimit)
	After *After // nil: first page
}

// OwnQuery is listMySecurityEvents.
type OwnQuery struct{ PageQuery }

// OrgQuery is listOrgAuditEvents (04 §2.21).
type OrgQuery struct {
	PageQuery
	From, To    time.Time // required, To exclusive
	Action      string
	ActorUserID *uuid.UUID
}

// AdminQuery is adminListAuditEvents (04 §2.21).
type AdminQuery struct {
	PageQuery
	From, To     time.Time // required, To exclusive
	ActorUserID  *uuid.UUID
	OrgID        *uuid.UUID
	ResourceType string
	ResourceID   *uuid.UUID
	Action       string
	Outcome      Outcome
	BreakGlass   *bool
}

// View is an event as a reader may see it (AuditEventDTO). Fields the
// reader may not see are empty: IP/User-Agent outside dpo/platform_admin,
// metadata outside adminListAuditEvents, and everything but the existence
// (action, outcome, actor, time, reason) of talent.* events (03 §5.21).
type View struct {
	ID           uuid.UUID
	Action       string
	Outcome      Outcome
	ActorUserID  *uuid.UUID
	ActorDisplay string
	ResourceType string
	ResourceID   *uuid.UUID
	Reason       string
	OccurredAt   time.Time
	IP           string
	UserAgent    string
	Metadata     json.RawMessage // nil when hidden
}

// SecurityEvent is one item of listMySecurityEvents: no full IP or
// User-Agent, and no network facts at all for a request by someone else
// (break-glass, a sign-in attempt against the account). A break-glass
// item carries the staff member's justification (BE-091).
//
// A sign-in attempt item (Attempts > 0, BE-078) summarises consecutive
// refused sign-ins to the account with the same reason, each at most
// attemptGap before the next: ID and OccurredAt are the newest one's,
// FirstOccurredAt the oldest one's.
type SecurityEvent struct {
	ID              uuid.UUID
	Action          string
	Outcome         Outcome
	OccurredAt      time.Time
	IPPrefix        string // /24 (IPv4) or /48 (IPv6)
	UserAgentFamily string
	// Reason is a reason code (invalid_credentials, locked, …), or for a
	// break-glass access (BreakGlass) the staff member's justification,
	// free text (T-326, BE-091).
	Reason          string
	BreakGlass      bool // a break-glass access to the user's data by staff (03 §6.3)
	Attempts        int  // sign-in attempts summarised by the item; 0 for any other event
	FirstOccurredAt time.Time
}

// VerifyResult is the outcome of a chain verification (04 §2.21).
type VerifyResult struct {
	OK               bool
	CheckedRows      int64
	FirstBadChainSeq *int64
	// Problem says what broke (gap, prev_hash, row_hash, root, signature,
	// archive); for logs and the alarm, never shown to clients.
	Problem string
}
