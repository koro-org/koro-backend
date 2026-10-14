// Package catalog is the catalog module (01 §4.17): the Koro service
// catalogue (services: what may be sold, with its invoice wording and the
// accountant's VAT profile), offerings (teklif = paket in v1: a seller org
// — a club, a university or the platform — sells a service, optionally at
// one of its events, at a price, with deliverables and a stock), their
// approval (platform admin, and the university when required) and the
// commerce policies (money model A/A2/B/C and commission, by scope and
// validity; ADR-0014). It is the first step of "selling packages to
// companies on an organisation by choosing a service" (meeting 2 decision
// #4): orders (BE-040) buy offerings, reserve their stock here and freeze
// the policy in effect onto each order line.
//
// Rules the package enforces:
//
//   - Org isolation (03 §3.6, §7.6, 06 H-38): /orgs/{org_id}/... is the
//     org's management surface (non-members 404); /offerings/{id}/... is a
//     resource surface: a published offering is every signed-in user's, an
//     unpublished one only its seller's, the platform's and — while it
//     waits for its decision — its university's (anyone else 404). The
//     seller org, the event and the payee of an offering come from the rows
//     (the event and the payee must be the seller's: 422
//     catalog.parent_mismatch, and the composite FKs of 000099, 06 H-55).
//   - Four eyes (03 §6.1, k24): the creator of an offering never approves
//     it (authz core table on created_by; offerings_approver_chk in the
//     database, answered as 403 authz.four_eyes_violation).
//   - Selling needs a reviewed service and a payable payee (BE-037): an org
//     offering is submitted — and goes on sale — only with an active
//     service the accountant reviewed (409 catalog.service_not_sellable),
//     an active payee of the seller (409 catalog.payee_not_active) that has
//     an approved bank account (409 parties.no_approved_account), and a
//     commerce policy in effect (409 catalog.no_commerce_policy; T-10).
//   - Money (ADR-0014, 06 H-6): prices are int64 kuruş, rates basis points;
//     the VAT rate is copied from the service (at creation, on a service
//     change and at submission); the VAT class and withholding stay on the
//     service and reach orders through ForCheckout. Commission arithmetic
//     is platform/money only (CommercePolicy.LineCommission).
//   - Stock (02 §18.2): Reserve is one conditional UPDATE (the last unit
//     turns the offering sold_out) under the offering's row lock, which
//     also serialises the per_buyer_limit check (BuyerHeld, BE-040).
//   - Commerce policies (02 §18.3, 06 H-17): a row never changes, it is
//     only closed (valid_to); a change is "close + new row"; periods of one
//     scope never overlap (409 catalog.policy_overlap, T-43). No policy
//     starts or ends in the past (resolution of past orders is stable).
//     Creating and closing need re-authentication (03 §6.2), are audited
//     and every platform and finance admin is notified (A task). There is
//     no default rate in code: `koro seed bootstrap-commerce` writes the
//     development default (model A, 10 % from the seller, net of VAT);
//     production gets its rate only from the team (T-10).
//   - Events (01 §6.2): offering.submitted and offering.decided (A) notify
//     the approvers and the seller; offering.cancelled (O) — also when the
//     bound event is cancelled (event.cancelled consumer) — lets orders
//     cancel unpaid orders and queue refunds (BE-040).
//   - Personal data: none of the three tables holds any (02 §18; staff
//     references only), so catalog registers no KVKK provider.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Offerings is the contract of orders (01 §4.17, §6.3 "Sipariş verme"). It
// takes no actor (trusted in-process calls; orders authorises its own
// operation) and joins the caller's transaction.
type Offerings interface {
	// ForCheckout returns what an order line needs of an offering on sale:
	// price, VAT profile, seller, default payee, deliverables, fulfilment and
	// invoice trigger. ErrNotFound, ErrNotOnSale (not active or outside its
	// sales window) or ErrSoldOut.
	ForCheckout(ctx context.Context, offeringID uuid.UUID) (CheckoutOffering, error)
	// Reserve takes qty units for buyerOrgID with one conditional UPDATE
	// under the offering's row lock: ErrNotOnSale, ErrSoldOut or
	// ErrPerBuyerLimit (BuyerHeld + qty > per_buyer_limit). orders calls it
	// in the order's transaction (01 §6.3; sync_writes.txt).
	Reserve(ctx context.Context, offeringID uuid.UUID, qty int, buyerOrgID uuid.UUID) error
	// Release gives qty units back (an expired or cancelled order), in the
	// caller's transaction (sync_writes.txt: orders -> catalog.Offerings.Release).
	Release(ctx context.Context, offeringID uuid.UUID, qty int) error
	// ResolvePolicy returns the commerce policy in effect for the offering at
	// t: offering > service > seller_org > default (02 §18.3); ErrNoPolicy
	// when none applies.
	ResolvePolicy(ctx context.Context, offeringID uuid.UUID, at time.Time) (CommercePolicy, error)
	// InvoiceTerms returns how billing invoices a line of the offering
	// (BE-042): the offering's invoice trigger, the service's invoice text
	// and VAT codes (any status: the line may be long sold) and the invoice
	// automation of the commerce policy frozen on the line (policyID).
	// ErrNotFound for an unknown offering, service or policy.
	InvoiceTerms(ctx context.Context, offeringID, policyID uuid.UUID) (InvoiceTerms, error)
}

// Service is the catalog service: the cross-module contract plus the HTTP
// use cases of 04 §2.14. Every method that takes an actor calls authz.Can
// first (05 §5.3).
type Service interface {
	Offerings

	// ListServices is listServices (catalog.service.list): active and
	// reviewed services; finance and platform admins see every service.
	ListServices(ctx context.Context, a authz.Actor, category Category) ([]ServiceItem, error)
	// AdminCreateService is adminCreateService (catalog.service.manage; the
	// accountant's review is finance's only, 03 k23).
	AdminCreateService(ctx context.Context, a authz.Actor, in ServiceInput) (ServiceItem, error)
	// AdminUpdateService is adminUpdateService (catalog.service.manage, k23).
	AdminUpdateService(ctx context.Context, a authz.Actor, id uuid.UUID, p ServicePatch) (ServiceItem, error)

	// BrowseOfferings is browseOfferings (catalog.offering.browse): the
	// active offerings whose sales have not ended.
	BrowseOfferings(ctx context.Context, a authz.Actor, q BrowseQuery) (OfferingPage, error)
	// GetOffering is getOffering (catalog.offering.read).
	GetOffering(ctx context.Context, a authz.Actor, id uuid.UUID) (OfferingView, error)
	// ListOrgOfferings is listOrgOfferings (catalog.offering.list_org).
	ListOrgOfferings(ctx context.Context, a authz.Actor, orgID uuid.UUID, q OrgQuery) (OfferingPage, error)
	// CreateOffering is createOffering (catalog.offering.create): a draft.
	CreateOffering(ctx context.Context, a authz.Actor, orgID uuid.UUID, in OfferingInput) (OfferingView, error)
	// UpdateOffering is updateOffering (catalog.offering.update): drafts
	// only; ifMatch is the If-Match header (missing → 428, stale → 412).
	UpdateOffering(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p OfferingPatch) (OfferingView, error)
	// SubmitOffering is submitOffering (catalog.offering.submit): draft →
	// pending_approval.
	SubmitOffering(ctx context.Context, a authz.Actor, id uuid.UUID) (OfferingView, error)
	// CancelOffering is cancelOffering (catalog.offering.cancel).
	CancelOffering(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (OfferingView, error)
	// AdminReviewQueue is adminListOfferingReviewQueue
	// (catalog.offering.review_queue).
	AdminReviewQueue(ctx context.Context, a authz.Actor, q QueueQuery) (OfferingPage, error)
	// AdminDecideOffering is adminDecideOffering (catalog.offering.decide).
	AdminDecideOffering(ctx context.Context, a authz.Actor, id uuid.UUID, d Decision) (OfferingView, error)
	// ListUniversityApprovals is listUniversityOfferingApprovals
	// (catalog.offering.review_queue on a university org).
	ListUniversityApprovals(ctx context.Context, a authz.Actor, orgID uuid.UUID, q QueueQuery) (OfferingPage, error)
	// DecideUniversityApproval is decideUniversityOfferingApproval
	// (catalog.offering.university_decide).
	DecideUniversityApproval(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, d Decision) (OfferingView, error)

	// AdminListPolicies is adminListCommercePolicies (catalog.policy.list).
	AdminListPolicies(ctx context.Context, a authz.Actor, f PolicyFilter) ([]CommercePolicy, error)
	// AdminCreatePolicy is adminCreateCommercePolicy (catalog.policy.manage, R).
	AdminCreatePolicy(ctx context.Context, a authz.Actor, in PolicyInput, proof identity.ReauthProof) (CommercePolicy, error)
	// AdminClosePolicy is adminCloseCommercePolicy (catalog.policy.manage,
	// R): validTo nil closes it now.
	AdminClosePolicy(ctx context.Context, a authz.Actor, id uuid.UUID, validTo *time.Time, proof identity.ReauthProof) (CommercePolicy, error)
}

// Deps are the dependencies of the catalog module. Store, Tx, Audit,
// Idempotency, Orgs, Outbox, Queue, Clock and Log are required (New returns
// an error otherwise).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Audit       AuditLogger
	Idempotency *idempotency.Keeper // optional Idempotency-Key of the POST operations (06 H-5)
	Orgs        Orgs
	Outbox      outbox.Writer // offering.cancelled (O) in the transaction; submitted/decided (A) after it
	Queue       queue.Enqueuer
	Clock       clock.Clock
	Log         *slog.Logger

	// Payees serves the payee rules of submission and approval. nil → those
	// fail closed with 503 (no org offering goes on sale unchecked).
	Payees Payees
	// Directory resolves a club's university (T-21) and a university's orgs.
	// nil → no offering can require a university's approval (422) and the
	// university filter of the marketplace finds nothing.
	Directory Directory
	// Events resolves the bound event. nil → binding an event is 503.
	Events Events
	// Reauth verifies the proof of catalog.policy.manage. nil → 503.
	Reauth Reauthenticator
	// BuyerHeld is the per-buyer count of orders (BE-040); nil → 0.
	BuyerHeld BuyerHeld
	// Sender, Users and Staff serve the notices in the worker; nil Sender or
	// Staff → the notice task fails and is retried; nil Users → in-app only.
	Sender Sender
	Users  Users
	Staff  Staff
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Audit", d.Audit != nil}, {"Idempotency", d.Idempotency != nil},
		{"Orgs", d.Orgs != nil}, {"Outbox", d.Outbox != nil}, {"Queue", d.Queue != nil}, {"Clock", d.Clock != nil},
		{"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("catalog.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what catalog hands to the composition root. The development
// default policy is written by NewSeeder (`koro seed bootstrap-commerce`).
type Module struct {
	Service Service
	// Offerings is the contract of orders (01 §4.17).
	Offerings Offerings

	svc *service
}

// New builds the catalog module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Offerings: s, svc: s}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
