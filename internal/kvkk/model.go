package kvkk

import (
	"slices"
	"time"
	"uuid"
)

// LegalPeriod is the answer period of a data subject request (KVKK md. 13:
// "en geç otuz gün içinde"): due_at = received_at + 30 days.
const LegalPeriod = 30 * 24 * time.Hour

// WarnBefore is when the DPO is warned about an open request (01 §7
// kvkk.dsr_deadlines: ≤ 7 days left).
const WarnBefore = 7 * 24 * time.Hour

// Type is data_subject_requests.type (02 §16.1; KVKK md. 11).
type Type string

// Request types.
const (
	TypeAccess             Type = "access"              // md. 11 (b, c): what is processed, for what
	TypeExport             Type = "export"              // a copy of the data (portability)
	TypeRectification      Type = "rectification"       // md. 11 (d)
	TypeErasure            Type = "erasure"             // md. 11 (e): account deletion (R)
	TypeRestriction        Type = "restriction"         // restrict processing
	TypeObjection          Type = "objection"           // md. 11 (g)
	TypeProfilingAccess    Type = "profiling_access"    // the talent auditor report (four eyes)
	TypeProfilingObjection Type = "profiling_objection" // talent neutralises (dsr.profiling_objection)
	TypeInfo               Type = "info"                // md. 11 (a, ç, f)
)

// Types lists every request type in 02 order.
func Types() []Type {
	return []Type{TypeAccess, TypeExport, TypeRectification, TypeErasure, TypeRestriction,
		TypeObjection, TypeProfilingAccess, TypeProfilingObjection, TypeInfo}
}

// Valid reports whether t is a known type.
func (t Type) Valid() bool { return slices.Contains(Types(), t) }

// exports reports whether completing t produces a data export file.
func (t Type) exports() bool {
	return t == TypeAccess || t == TypeExport || t == TypeProfilingAccess
}

// Channel is data_subject_requests.channel.
type Channel string

// Channels: in_app requests come from /me; the others are entered by the DPO.
const (
	ChannelInApp    Channel = "in_app"
	ChannelEmail    Channel = "email"
	ChannelPost     Channel = "post"
	ChannelKEP      Channel = "kep"
	ChannelInPerson Channel = "in_person"
)

// Valid reports whether c is a known channel.
func (c Channel) Valid() bool {
	return slices.Contains([]Channel{ChannelInApp, ChannelEmail, ChannelPost, ChannelKEP, ChannelInPerson}, c)
}

// Status is data_subject_requests.status.
type Status string

// Request statuses.
const (
	StatusReceived       Status = "received"
	StatusIdentityCheck  Status = "identity_check"
	StatusInProgress     Status = "in_progress"
	StatusAwaitingSecond Status = "awaiting_second_approval"
	StatusCompleted      Status = "completed"
	StatusRejected       Status = "rejected"
	StatusCancelled      Status = "cancelled"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	return slices.Contains([]Status{StatusReceived, StatusIdentityCheck, StatusInProgress, StatusAwaitingSecond,
		StatusCompleted, StatusRejected, StatusCancelled}, s)
}

// Open reports whether the request still runs against its deadline.
func (s Status) Open() bool {
	return s != StatusCompleted && s != StatusRejected && s != StatusCancelled
}

// workable reports whether the DPO may move, complete or approve (first
// stage) the request: open and not waiting for the second approver.
func (s Status) workable() bool {
	return s == StatusReceived || s == StatusIdentityCheck || s == StatusInProgress
}

// Outcomes of dsr.completed (domainevent.DSRCompleted.Outcome).
const (
	OutcomeAnswered  = "answered"   // the DPO answered (response_summary)
	OutcomeExport    = "export"     // a data export is built, then the subject is notified
	OutcomeErased    = "erased"     // the account was erased
	OutcomeNoAccount = "no_account" // the applicant has no account: nothing to export or erase
	OutcomeRejected  = "rejected"
)

// DSR is one data_subject_requests row (02 §16.1).
type DSR struct {
	ID                 uuid.UUID
	UserID             *uuid.UUID // nil: unregistered applicant entered by the DPO
	RequesterName      string     // only when UserID is nil
	RequesterEmail     string     // only when UserID is nil
	Type               Type
	Channel            Channel
	Status             Status
	Details            string
	ReceivedAt         time.Time
	DueAt              time.Time
	IdentityVerifiedAt *time.Time
	AssignedTo         *uuid.UUID
	FirstApprover      *uuid.UUID
	SecondApprover     *uuid.UUID
	ResponseSummary    string
	ExportFileID       *uuid.UUID
	RejectionReason    string
	CompletedAt        *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// DaysLeft returns the whole days until due_at at now, rounded up; zero or
// negative once the deadline has passed.
func (d DSR) DaysLeft(now time.Time) int {
	left := d.DueAt.Sub(now)
	days := int(left / (24 * time.Hour))
	if left > 0 && left%(24*time.Hour) != 0 {
		days++
	}
	return days
}

func (d DSR) clone() DSR {
	c := d
	for _, p := range []**uuid.UUID{&c.UserID, &c.AssignedTo, &c.FirstApprover, &c.SecondApprover, &c.ExportFileID} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	for _, p := range []**time.Time{&c.IdentityVerifiedAt, &c.CompletedAt} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	return c
}

// After is a keyset cursor position.
type After struct {
	At time.Time
	ID uuid.UUID
}

// AdminFilter filters adminListDataRequests (04 §2.22). Statuses empty =
// every status.
type AdminFilter struct {
	Statuses  []Status
	Type      *Type
	DueBefore *time.Time
}

// Page is one page of requests and the position after it (nil: last page).
type Page struct {
	Items []DSR
	Next  *After
}
