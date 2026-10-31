package httpx

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
)

// The terms gate (BE-124, user decision Q-17): accepting the terms of use
// in force is the contract, so a signed-in user who has not accepted the
// current version (a new one was published, or the account predates the
// ledger) may read but not write until acceptTerms records it. The rule is
// enforced here, once, for every operation, instead of in each client
// (koro-web's TermsGate, an old mobile release) or in each service.
//
// Why an operation middleware and not authz.Can: the rule is about the
// request (a write method on a protected surface), not about an action —
// authz actions are shared by reads and writes and by acceptTerms and
// updateMe (identity.account.update_own), so no action list could tell the
// way out of the gate from a profile edit. The middleware runs after
// authentication and the operation's rate limit and before the input is
// parsed, so a refused write costs no handler work (cmd/koro apiMiddleware).
//
// Which operations: every write method (anything but GET, HEAD and
// OPTIONS) on a protected surface (Security set: self, org, resource,
// admin). Public operations (login, registration, the e-mail token links,
// one-click unsubscribe) and webhooks (a provider's signature, never a
// user) have no signed-in user to hold to a contract. A protected write is
// gated unless termsExemptOperations lists it, so a new write operation is
// gated by default; installTermsGate writes the decision into the spec
// (ExtTermsGate) and refuses an exemption that does not match its
// operation.

// TermsChecker answers the gate for one user (identity, BE-124: the terms
// in force for the user's audience and locale and the user's acceptance,
// cached per user and per version).
type TermsChecker interface {
	// CheckTerms returns nil when userID may write: they accepted the terms
	// of use in force for them, or none is published. A problem error
	// (403 consent.terms_acceptance_required) is the gate's answer; any
	// other error is a server failure (500).
	CheckTerms(ctx context.Context, userID uuid.UUID) error
}

// ExtTermsGate is the extension of every protected write operation:
// TermsGateEnforced (a user who has not accepted the current terms of use
// gets 403 consent.terms_acceptance_required) or TermsGateExempt.
const ExtTermsGate = "x-koro-terms-gate"

// Values of ExtTermsGate.
const (
	TermsGateEnforced = "enforced"
	TermsGateExempt   = "exempt"
)

// termsExemptOperations are the protected write operations a user may call
// without having accepted the current terms of use (operationId →
// "METHOD /api/v1/path"; the row must match its operation exactly, or
// registration panics). Each group says why; a new row needs a reason of
// the same kind: the way out of the gate, leaving, account security, a
// KVKK right, a consent act, a communication preference or the user's own
// protection (BE-152, Q-20) or a read sent as POST. Everything else a user
// writes rests on the contract — staff, admin and DPO writes included
// (Q-20: no separate exemption for staff).
var termsExemptOperations = map[string]string{
	// The way out of the gate: accepting the current version.
	"acceptTerms": "POST " + APIPrefix + "/me/terms-acceptances",

	// Leaving: a user who does not accept the new terms can always end the
	// session (logout, another device's session). deleteMyPushToken is the
	// mobile logout's first step: the device stops receiving pushes.
	"deleteCurrentSession": "DELETE " + APIPrefix + "/auth/sessions/current",
	"revokeMySession":      "DELETE " + APIPrefix + "/me/sessions/{session_id}",
	"deleteMyPushToken":    "DELETE " + APIPrefix + "/me/push-tokens/{push_token_id}",

	// Account security (05 §14, 03 §6.4): step-up, the second factor, the
	// password and the address an account is recovered with. Securing a
	// possibly compromised account never waits for a contract, and an MFA
	// role's session needs the step-up to reach acceptTerms' answer at all.
	"verifyMfa":               "POST " + APIPrefix + "/me/mfa/verify",
	"enrollTotp":              "POST " + APIPrefix + "/me/mfa/totp",
	"confirmTotp":             "POST " + APIPrefix + "/me/mfa/totp/confirm",
	"disableTotp":             "POST " + APIPrefix + "/me/mfa/totp/disable",
	"regenerateRecoveryCodes": "POST " + APIPrefix + "/me/mfa/recovery-codes",
	"changeMyPassword":        "POST " + APIPrefix + "/me/password",
	"requestEmailChange":      "POST " + APIPrefix + "/me/email-change",

	// KVKK rights of the data subject (Law 6698 m.11, m.13): an
	// application — access, rectification, erasure (the account's
	// deletion), objection — and its withdrawal are never conditional on
	// accepting a contract.
	"createMyDataRequest": "POST " + APIPrefix + "/me/data-requests",
	"cancelMyDataRequest": "POST " + APIPrefix + "/me/data-requests/{dsr_id}/cancel",

	// Consent acts (Law 6698 m.5, Kurul 2026/347: an explicit consent is
	// never bundled with the service contract): a consent is granted or
	// withdrawn on its own, at any time — commercial e-mail and push
	// opt-out (Law 6563 m.8) included. updateMyPrivacy is the mirror of the
	// discovery_profile and contact_info_share consents (turning one off
	// withdraws it); revoking an access grant or un-sharing its contact
	// withdraws the permission a company holds to the student's profile
	// and e-mail.
	"recordMyConsent":             "POST " + APIPrefix + "/me/consents",
	"updateMyPrivacy":             "PATCH " + APIPrefix + "/me/privacy",
	"revokeMyAccessGrant":         "POST " + APIPrefix + "/me/access-grants/{grant_id}/revoke",
	"unshareMyAccessGrantContact": "DELETE " + APIPrefix + "/me/access-grants/{grant_id}/contact",

	// Communication preferences (BE-152, Q-20; Law 6563 m.8, Ticari
	// İletişim Yön.): refusing a message is always possible, so the
	// notification preferences (every category × channel; the security
	// category stays locked on by notify, 422
	// notify.security_preference_locked) never wait for a contract.
	"putMyNotificationPreferences": "PUT " + APIPrefix + "/me/notification-preferences",

	// Trust and safety (BE-152, Q-20): blocking a company's contact
	// requests is the user protecting themselves; the block list (and
	// undoing a mistaken block) is the user's own setting, like the
	// preferences above. Answering a request or cancelling a registration
	// stays gated: it is using the service, not refusing it.
	"blockCompany":   "PUT " + APIPrefix + "/me/company-blocks/{org_id}",
	"unblockCompany": "DELETE " + APIPrefix + "/me/company-blocks/{org_id}",

	// Reads sent as POST (BE-128): a search whose criteria name a person
	// (an e-mail address, a name, a user ID) takes them in the body so they
	// stay out of URLs and access logs (httpx personal query guard). The
	// operation is a list read under its read action (the GET lists it
	// replaced are gone, BE-133): it changes nothing (the audit row of the
	// read aside), and a read is never behind the gate.
	"adminQueryUsers":             "POST " + APIPrefix + "/admin/users/search",
	"adminQueryEmailSuppressions": "POST " + APIPrefix + "/admin/email-suppressions/search",
	"adminQueryAuditEvents":       "POST " + APIPrefix + "/admin/audit-events/search",
	"queryOrgAuditEvents":         "POST " + APIPrefix + "/orgs/{org_id}/audit-events/search",
	"queryCompanyGrantedStudents": "POST " + APIPrefix + "/orgs/{org_id}/students/search",
	"adminQueryIncomingTransfers": "POST " + APIPrefix + "/admin/incoming-transfers/search",
	"adminQueryLegalParties":      "POST " + APIPrefix + "/admin/legal-parties/search",
	// BE-129: the staff's payee search (a legal party's title may name a
	// sole proprietor); it has no GET twin, the read is its own.
	"adminQueryPayees": "POST " + APIPrefix + "/admin/payees/search",
	// BE-136: the price preview of a cart takes createOrder's body (the cart:
	// up to 20 structured lines, no query string) and writes nothing — no
	// stock, no order, no Idempotency-Key. Placing the order stays gated.
	"quoteOrder": "POST " + APIPrefix + "/orgs/{org_id}/orders/quote",
}

// TermsExemptOperations returns a copy of the exemption table (the spec
// test of cmd/koro checks it against every registered operation).
func TermsExemptOperations() map[string]string {
	return maps.Clone(termsExemptOperations)
}

// protectedWrite reports a write method on a protected surface.
func protectedWrite(op *huma.Operation) bool {
	if len(op.Security) == 0 {
		return false
	}
	switch op.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// termsExempt reports an operation listed in termsExemptOperations with
// its own method and path.
func termsExempt(op *huma.Operation) bool {
	want, listed := termsExemptOperations[op.OperationID]
	return listed && want == op.Method+" "+op.Path
}

// termsGated reports whether the gate covers op.
func termsGated(op *huma.Operation) bool {
	return protectedWrite(op) && !termsExempt(op)
}

// installTermsGate marks every protected write operation of oapi with
// ExtTermsGate when it is added (huma's OnAddOperation) and panics on an
// exemption row that names its operation with another method or path, or
// that names an operation the gate never covers (a read, a public or
// webhook operation): `koro api`, `koro openapi` and the tests register
// every operation, so a stale row never ships.
func installTermsGate(oapi *huma.OpenAPI) {
	oapi.OnAddOperation = append(oapi.OnAddOperation, func(_ *huma.OpenAPI, op *huma.Operation) {
		if want, listed := termsExemptOperations[op.OperationID]; listed {
			switch got := op.Method + " " + op.Path; {
			case got != want:
				panic(fmt.Sprintf("httpx: terms gate exemption %s is %q, the operation is %q", op.OperationID, want, got))
			case !protectedWrite(op):
				panic(fmt.Sprintf("httpx: terms gate exemption %s is not a protected write operation", op.OperationID))
			}
		}
		if !protectedWrite(op) {
			return
		}
		if op.Extensions == nil {
			op.Extensions = map[string]any{}
		}
		op.Extensions[ExtTermsGate] = TermsGateEnforced
		if termsExempt(op) {
			op.Extensions[ExtTermsGate] = TermsGateExempt
		}
	})
}

// TermsExemptionsUnregistered returns the exemption rows whose operation is
// not in oapi, sorted (a renamed or removed operation leaves its row
// behind; the spec test fails on it).
func TermsExemptionsUnregistered(oapi *huma.OpenAPI) []string {
	seen := map[string]bool{}
	for _, op := range operationsOf(oapi) {
		if termsExempt(op) {
			seen[op.OperationID] = true
		}
	}
	var out []string
	for id := range termsExemptOperations {
		if !seen[id] {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// TermsGate is the operation middleware of the terms gate: on a gated
// operation (termsGated) it asks terms about the signed-in user and
// answers its error instead of running the operation. The authentication
// middleware has already answered 401 to a protected operation without a
// user, so a missing actor here only passes through (the handler's own
// authz.ActorFrom answers it). Install it on the root API after the rate
// limit and before Idempotency-Key parsing (cmd/koro apiMiddleware).
func TermsGate(api huma.API, terms TermsChecker) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		if op := ctx.Operation(); op == nil || !termsGated(op) {
			next(ctx)
			return
		}
		a, err := authz.ActorFrom(ctx.Context())
		if err != nil || !a.IsUser() {
			next(ctx)
			return
		}
		if err := terms.CheckTerms(ctx.Context(), a.UserID); err != nil {
			// A problem error keeps its own status (403); anything else is
			// a logged 500 (toProblem).
			_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "", err)
			return
		}
		next(ctx)
	}
}
