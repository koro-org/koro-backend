// Package messaging is the contact request of companies (01 §4.15, 02 §14,
// 03 §5.9, 04 §2.9; BE-056): a company that found a student in discovery
// (under its pseudonym, anon_id) asks to get in touch; the student accepts
// (the company gets level 3 access in the same transaction) or declines
// without a reason; an unanswered request expires after 14 days; the
// student may block a company. Conversations are phase 2.
//
// Rules the package enforces:
//
//   - Who (03 §5.9): the recruiter (cmm) and the company_admin/owner (cma,
//     MFA) of the company send, list, read and withdraw on the company's
//     surface; the student answers, reads and blocks on /me (O). Members of
//     the org without one of these roles and every platform role see the
//     company surface (403), everyone else 404 (03 §7.6).
//   - k19 (03 §5.9): the company must be active (403 orgs.org_suspended,
//     orgs.org_not_verified); the target must be discoverable now (level 2
//     on with its consent, an active verification, an adult, an active
//     account: students.Discoverable), must not have blocked the company
//     and must not have declined it within 90 days. A target that does not
//     qualify is the same 404 as an unknown pseudonym: the student's
//     existence and choices never leak. One contact_request unit is
//     consumed in the request's transaction (403 entitlements.not_entitled
//     without one) and never given back (withdrawal, decline, expiry).
//   - Idempotency-Key is required on creation (04 §1.8): the key, the unit,
//     the row, the outbox event and the audit record share one transaction
//     (06 H-5); a replay consumes nothing.
//   - The company sees the student only under its pseudonym until the
//     student accepts (04 §2.9: then student_user_id, the level 3 link).
//     The acceptance gives level 3 access through students.AccessGrants in
//     the same transaction (01 §6.3) and needs an active student
//     verification (403 students.verification_required, BE-077); a decline
//     never does and carries no reason.
//   - A block declines the company's pending requests (the company sees an
//     ordinary decline and never learns of the block) and hides the student
//     from its new requests (404).
//   - share_contact (T-335, T-345; BE-089): an acceptance may share the
//     student's e-mail with the request's company alone, in the same
//     transaction (students.ContactSharing: contact_shared_at of the
//     company's grant, the contact_info_share consent with the text shown).
//     No other company starts seeing it; the student takes it back for that
//     company (unshareMyAccessGrantContact), ends the company's access
//     (revokeMyAccessGrant) or stops every share (updateMyPrivacy
//     show_contact_on_grant: false). A sharing that cannot be written rolls
//     the acceptance back.
//   - Notices (01 §6.2): contact_request.created → the student;
//     contact_request.closed → the sender (accepted, declined or expired;
//     never a reason). Each goes in-app, by push and by e-mail (T-336;
//     BE-089); notify skips a channel the recipient switched off
//     (contact_requests, 02 §6.5). In-app and push texts are fixed, their
//     data the request's ID; the e-mail adds the company's name (to the
//     student) and a link to the request. The subject and the message (the
//     company's free text) never enter an event or a notification: a
//     transactional e-mail carries no promotional content (Law 6563).
//   - messaging.expire_requests (01 §7, hourly at :40) closes the pending
//     requests past their 14 days; until it runs they already read as
//     expired and cannot be answered. Closed requests are hard deleted 12
//     months after they closed (contact_request_closed; updated_at of a
//     closed row is the moment it closed).
//   - Personal data: logs carry IDs only; the KVKK provider exports the
//     requests a user received or sent and the blocks, and erases the
//     received requests and the blocks (sent requests are the company's
//     record and point at the anonymised account).
package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
	"koro/internal/students"
)

const (
	// RequestTTL is how long a request waits for the answer (02 §14.1).
	RequestTTL = 14 * 24 * time.Hour
	// DeclineCooldown is how long a company may not ask a student who
	// declined it again (03 k19).
	DeclineCooldown = 90 * 24 * time.Hour
)

// CreateInput is createContactRequest (04 §2.9).
type CreateInput struct {
	AnonID            string
	Subject           string
	Message           string
	OpportunityPostID *uuid.UUID
}

// CompanyView is a request as its company sees it (ContactRequestCompanyDTO):
// the student under the company's pseudonym, the effective status.
type CompanyView struct {
	Request ContactRequest
	AnonID  string
	Status  Status
}

// CompanyPage is a page of listSentContactRequests.
type CompanyPage struct {
	Items []CompanyView
	Next  *uuid.UUID
}

// PostRef is the opportunity call a request refers to; Title is set while
// the post is published.
type PostRef struct {
	ID    uuid.UUID
	Title string
}

// StudentView is a request as its student sees it (ContactRequestStudentDTO).
type StudentView struct {
	Request ContactRequest
	Company orgs.OrgSummary // zero when the org is gone
	Post    *PostRef
	Status  Status
}

// StudentPage is a page of listMyContactRequests.
type StudentPage struct {
	Items []StudentView
	Next  *uuid.UUID
}

// RespondInput is respondMyContactRequest (04 §2.9).
type RespondInput struct {
	Decision Decision
	// ShareContact shares the student's e-mail with the request's company
	// alone, with the acceptance (T-335/T-345): the company's grant and the
	// contact_info_share consent in the acceptance's transaction. Only with
	// DecisionAccepted.
	ShareContact bool
	// ContactConsent is the contact_info_share text the student was shown;
	// required with ShareContact.
	ContactConsent *students.ConsentProof
	// Channel is the consent channel (web | mobile; X-Koro-Client).
	Channel consent.Channel
}

// BlockView is one of the student's blocks with the company's card.
type BlockView struct {
	Block   Block
	Company orgs.OrgSummary
}

// BlockPage is a page of listMyCompanyBlocks.
type BlockPage struct {
	Items []BlockView
	Next  *BlockKey
}

// Service is the messaging service: the 10 HTTP use cases of 04 §2.9.
// Every method calls authz.Can first (05 §5.3).
type Service interface {
	// Create is createContactRequest (messaging.contact_request.create).
	Create(ctx context.Context, a authz.Actor, orgID uuid.UUID, in CreateInput) (CompanyView, error)
	// ListSent is listSentContactRequests (messaging.contact_request.list_sent).
	ListSent(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ListQuery) (CompanyPage, error)
	// GetSent is getSentContactRequest (messaging.contact_request.read).
	GetSent(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (CompanyView, error)
	// Withdraw is withdrawContactRequest (messaging.contact_request.withdraw):
	// a pending request only; the unit stays used.
	Withdraw(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (CompanyView, error)

	// ListMine is listMyContactRequests (messaging.contact_request.list_received).
	ListMine(ctx context.Context, a authz.Actor, q ListQuery) (StudentPage, error)
	// GetMine is getMyContactRequest (messaging.contact_request.read).
	GetMine(ctx context.Context, a authz.Actor, id uuid.UUID) (StudentView, error)
	// Respond is respondMyContactRequest (messaging.contact_request.respond).
	Respond(ctx context.Context, a authz.Actor, id uuid.UUID, in RespondInput) (StudentView, error)

	// Block is blockCompany (messaging.block.manage): idempotent; the
	// company's pending requests are declined.
	Block(ctx context.Context, a authz.Actor, orgID uuid.UUID) error
	// Unblock is unblockCompany (messaging.block.manage): idempotent.
	Unblock(ctx context.Context, a authz.Actor, orgID uuid.UUID) error
	// ListBlocks is listMyCompanyBlocks (messaging.block.list_own).
	ListBlocks(ctx context.Context, a authz.Actor, after *BlockKey, limit int) (BlockPage, error)
}

// Deps are the dependencies of the messaging module. Resolve, Posts,
// Sender, Users and LinkBaseURL are optional (see their fields); the others
// are required.
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Outbox      outbox.Writer       // contact_request.created, contact_request.closed (O)
	Audit       AuditLogger         // every write, same transaction
	Idempotency *idempotency.Keeper // Idempotency-Key of createContactRequest (required) and the others (optional)
	Clock       clock.Clock
	Log         *slog.Logger

	Entitlements Entitlements   // the contact_request unit (same transaction)
	Grants       AccessGrants   // level 3 access on acceptance (same transaction)
	Contacts     ContactSharing // share_contact on acceptance (same transaction)
	Students     Discoverable   // the target qualifies now (k19)
	Orgs         Orgs           // the company's status, names, notice recipients
	Pseudonyms   Pseudonyms     // the company's anon_id of a student (discovery)

	// Resolve maps the company's pseudonym to the student (hooks.anonResolver,
	// discovery). nil → no pseudonym resolves: every request is 404.
	Resolve AnonResolver
	// Posts reads the opportunity call of a request (feed). nil → a
	// request naming a post is refused (422) and the student's view shows
	// the post without its title.
	Posts Posts
	// Sender delivers the notices (notify). nil → a notice task fails and
	// is retried; nothing is dropped.
	Sender Sender
	// Users reads the address of a notice's e-mail (identity). nil → the
	// notices go in-app and by push only.
	Users Users
	// LinkBaseURL is the web origin of the e-mail's link to the request
	// (IDENTITY_LINK_BASE_URL); required with Users.
	LinkBaseURL string
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Outbox", d.Outbox != nil}, {"Audit", d.Audit != nil},
		{"Idempotency", d.Idempotency != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
		{"Entitlements", d.Entitlements != nil}, {"Grants", d.Grants != nil}, {"Contacts", d.Contacts != nil},
		{"Students", d.Students != nil}, {"LinkBaseURL", d.Users == nil || d.LinkBaseURL != ""},
		{"Orgs", d.Orgs != nil}, {"Pseudonyms", d.Pseudonyms != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("messaging.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what messaging hands to the composition root.
type Module struct {
	Service Service
	// Privacy is the KVKK provider (contact_requests, contact_blocks).
	Privacy privacy.Provider
	// Sweeper deletes closed requests after their retention
	// (contact_request_closed; kvkk.retention_sweep runs it).
	Sweeper privacy.RetentionSweeper

	svc *service
}

// New builds the messaging module; a missing dependency is a wiring bug
// that stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Privacy: privacyProvider{s: s}, Sweeper: sweeper{s: s}, svc: s}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }

// emit writes an outbox event in the transaction carried by ctx.
func (s *service) emit(ctx context.Context, p domainevent.Payload, id uuid.UUID) error {
	env, err := domainevent.New(p, "contact_request", id)
	if err != nil {
		return err
	}
	return s.d.Outbox.Add(ctx, env)
}

// Subscribe registers the outbox subscriptions of messaging (01 §6.2):
// contact_request.created → the student's notice, contact_request.closed →
// the company's notice.
func (m Module) Subscribe(r *outbox.Registry) {
	q := outbox.Queue(queue.Default)
	r.Subscribe(domainevent.TopicContactRequestCreated, NotifyReceivedTaskName, m.svc.handleCreated, q)
	r.Subscribe(domainevent.TopicContactRequestClosed, NotifyClosedTaskName, m.svc.handleClosed, q)
}

// Jobs returns the scheduled jobs of messaging (01 §7): expire_requests.
func (m Module) Jobs() []queue.Job { return []queue.Job{m.svc.expireJob()} }
