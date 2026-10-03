package identity

import (
	"strings"
	"time"
	"uuid"
)

// EduStatus is edu_verifications.status (02 §5.4).
type EduStatus string

// Verification statuses. Only verified grants the student role, and only
// while expires_at is in the future (03 §2.1).
const (
	EduPending    EduStatus = "pending"    // a student document waits for a platform admin
	EduVerified   EduStatus = "verified"   // in effect until expires_at
	EduRejected   EduStatus = "rejected"   // a document refused by an admin
	EduExpired    EduStatus = "expired"    // identity.edu_expire: the year ran out without a renewal
	EduRevoked    EduStatus = "revoked"    // withdrawn after the fact (no v1 operation writes it)
	EduSuperseded EduStatus = "superseded" // replaced by a newer verification (renewal)
)

// EduMethod is edu_verifications.method.
type EduMethod string

// Verification methods.
const (
	EduMethodEmail    EduMethod = "edu_email" // a link sent to the .edu.tr address
	EduMethodDocument EduMethod = "document"  // a student document approved by a platform admin
)

// eduValidityYears is how long a verification lasts (01 §4.6: renewed yearly).
const eduValidityYears = 1

// EduRecord is one edu_verifications row.
type EduRecord struct {
	ID                 uuid.UUID
	UserID             uuid.UUID
	Method             EduMethod
	EduEmail           string // as entered; "" for documents
	EduEmailNormalized string
	EmailDomain        string
	UniversityID       *uuid.UUID // claimed (pending document) or verified university
	DocumentFileID     *uuid.UUID // documents; nil once the retention removed the file
	Status             EduStatus
	VerifiedAt         *time.Time
	ExpiresAt          *time.Time
	ReviewedBy         *uuid.UUID
	ReviewNote         string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// activeAt reports whether the row makes its user a verified student at
// now (03 §2.1: status verified AND expires_at > now).
func (r EduRecord) activeAt(now time.Time) bool {
	return r.Status == EduVerified && r.ExpiresAt != nil && now.Before(*r.ExpiresAt)
}

func (r EduRecord) clone() EduRecord {
	c := r
	c.UniversityID, c.DocumentFileID, c.ReviewedBy = cloneID(r.UniversityID), cloneID(r.DocumentFileID), cloneID(r.ReviewedBy)
	c.VerifiedAt, c.ExpiresAt = cloneTime(r.VerifiedAt), cloneTime(r.ExpiresAt)
	return c
}

// EduQuery is one page of the admin queue (adminListEduVerifications):
// student documents in Status; pending ones oldest first (a queue), the
// others newest first.
type EduQuery struct {
	Status EduStatus
	After  *uuid.UUID
	Limit  int
}

// newestFirst reports the order of a queue page.
func (q EduQuery) newestFirst() bool { return q.Status != EduPending }

// maskEduEmail shows an address as "a***@ogr.x.edu.tr" (04 §2.1: the
// .edu.tr address is never returned in full).
func maskEduEmail(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" {
		return ""
	}
	r := []rune(local)
	return string(r[0]) + "***@" + domain
}
