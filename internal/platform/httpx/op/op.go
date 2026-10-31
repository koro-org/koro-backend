// Package op writes the Koro OpenAPI extensions of an operation (04 §1.2)
// and derives its Security requirement from its surface:
//
//	huma.Register(api, op.Koro(huma.Operation{
//		OperationID: "createEvent", Method: http.MethodPost, Path: "/orgs/{org_id}/events",
//		Summary: "Create an event", Tags: []string{"events"},
//		DefaultStatus: http.StatusCreated,
//	}, op.Meta{
//		Action: "events.event.create", Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional,
//	}), h.create)
//
// Koro panics on an inconsistent operation: registration happens at process
// start, `koro openapi` and the tests run it, so mistakes never ship.
package op

import (
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx"
)

// Extension names (04 §1.2).
const (
	ExtAction      = "x-koro-action"
	ExtIdempotency = "x-koro-idempotency"
	ExtRateLimit   = "x-koro-rate-limit"
	ExtSurface     = "x-koro-surface"
	ExtReauth      = "x-koro-reauth"
	ExtBreakGlass  = "x-koro-break-glass"
)

// Surface decides the 404/403 rule of an operation (03 §7.6, 04 §1.4).
type Surface string

// Surfaces.
const (
	SurfacePublic   Surface = "public"
	SurfaceSelf     Surface = "self"
	SurfaceOrg      Surface = "org"
	SurfaceResource Surface = "resource"
	SurfaceAdmin    Surface = "admin"
	SurfaceWebhook  Surface = "webhook"
)

// Idempotency is the Idempotency-Key policy of a POST operation (04 §1.8).
type Idempotency string

// Idempotency policies.
const (
	IdempotencyOptional Idempotency = "optional"
	IdempotencyRequired Idempotency = "required"
)

// RateLimit is a rate limit class name (04 §1.9). The limits themselves and
// the middleware that reads x-koro-rate-limit belong to BE-006.
type RateLimit string

// Rate limit classes (04 §1.9). GlobalIP is applied to every request by a
// gin middleware and is not an operation class.
const (
	RateGlobalIP          RateLimit = "global_ip"
	RateAuthLogin         RateLimit = "auth_login"
	RateAuthRegister      RateLimit = "auth_register"
	RateAuthPasswordReset RateLimit = "auth_password_reset"
	RateAuthEmail         RateLimit = "auth_email"
	RateTokenConfirm      RateLimit = "token_confirm"
	RateReauth            RateLimit = "reauth"
	RateReadDefault       RateLimit = "read_default"
	RateWriteDefault      RateLimit = "write_default"
	RateSearch            RateLimit = "search"
	RateAudienceCount     RateLimit = "audience_count"
	RateContactRequest    RateLimit = "contact_request"
	RateQRScan            RateLimit = "qr_scan"
	RateTicketResend      RateLimit = "ticket_resend"
	RateExport            RateLimit = "export"
	RateBreakGlass        RateLimit = "break_glass"
	RateUpload            RateLimit = "upload"
	RateWebhook           RateLimit = "webhook"
	RateMessageSend       RateLimit = "message_send" // BE-162: a message of a conversation
)

// RateLimits lists every class of 04 §1.9 in table order.
func RateLimits() []RateLimit {
	return []RateLimit{
		RateGlobalIP, RateAuthLogin, RateAuthRegister, RateAuthPasswordReset, RateAuthEmail,
		RateTokenConfirm, RateReauth, RateReadDefault, RateWriteDefault, RateSearch,
		RateAudienceCount, RateContactRequest, RateQRScan, RateTicketResend, RateExport,
		RateBreakGlass, RateUpload, RateWebhook, RateMessageSend,
	}
}

// Meta carries the Koro-specific facts of an operation.
type Meta struct {
	// Action is the authz action (03 §5), "<module>.<entity>.<verb>".
	Action string
	// Surface selects the 404/403 rule and the Security requirement:
	// public and webhook operations have none, all others SessionOrBearer.
	Surface Surface
	// RateLimit is the operation class; empty means read_default for GET
	// and write_default otherwise.
	RateLimit RateLimit
	// Idempotency is mandatory for POST and forbidden for other methods.
	Idempotency Idempotency
	// Reauth marks "R" operations (body carries a reauth proof; 03 §6.2).
	Reauth bool
	// BreakGlass marks "B" operations (X-Koro-Access-Reason; 03 §6.3).
	BreakGlass bool
	// Responses documents the success answers the operation gives besides
	// its DefaultStatus (its output sets Status at run time; huma documents
	// the default status only). Register writes them (it holds the schema
	// registry); Koro alone refuses them.
	Responses []Response
}

// Response is a success answer of an operation besides its DefaultStatus,
// e.g. 202 with the open four-eyes request instead of the action (BE-128).
// Body is a value of the body type (nil: no body).
type Response struct {
	Status      int
	Description string
	Body        any
}

var (
	actionRe      = regexp.MustCompile(`^[a-z_]+\.[a-z_]+\.[a-z_]+$`)
	operationIDRe = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
)

// Koro validates o against m, sets Security and writes the x-koro-*
// extensions. It returns the updated operation for huma.Register.
// Meta.Responses needs Register.
func Koro(o huma.Operation, m Meta) huma.Operation {
	if len(m.Responses) > 0 {
		panic(fmt.Sprintf("op.Koro(%s %s %q): Meta.Responses needs op.Register", o.Method, o.Path, o.OperationID))
	}
	return koro(o, m)
}

// koro is Koro without the Register-only check.
func koro(o huma.Operation, m Meta) huma.Operation {
	fail := func(format string, args ...any) {
		panic(fmt.Sprintf("op.Koro(%s %s %q): ", o.Method, o.Path, o.OperationID) + fmt.Sprintf(format, args...))
	}
	if !operationIDRe.MatchString(o.OperationID) {
		fail("operationId must be lowerCamel")
	}
	if len(o.Tags) != 1 {
		fail("exactly one tag (the module name) is required")
	}
	if !actionRe.MatchString(m.Action) {
		fail("action %q must be <module>.<entity>.<verb>", m.Action)
	}

	switch m.Surface {
	case SurfacePublic, SurfaceWebhook:
		if len(o.Security) > 0 {
			fail("%s surface must not declare Security", m.Surface)
		}
		o.Security = nil
	case SurfaceSelf, SurfaceOrg, SurfaceResource, SurfaceAdmin:
		o.Security = httpx.SessionOrBearer
	default:
		fail("unknown surface %q", m.Surface)
	}

	rl := m.RateLimit
	if rl == "" {
		rl = RateWriteDefault
		if o.Method == http.MethodGet || o.Method == http.MethodHead {
			rl = RateReadDefault
		}
	}
	if rl == RateGlobalIP || !slices.Contains(RateLimits(), rl) {
		fail("unknown rate limit class %q", rl)
	}

	switch {
	case o.Method == http.MethodPost && m.Idempotency != IdempotencyOptional && m.Idempotency != IdempotencyRequired:
		fail("POST operations must declare idempotency optional|required")
	case o.Method != http.MethodPost && m.Idempotency != "":
		fail("idempotency applies to POST only")
	}

	def := o.DefaultStatus
	if def == 0 {
		def = http.StatusOK
	}
	seen := map[int]bool{def: true}
	for _, r := range m.Responses {
		if r.Status < 200 || r.Status > 299 || seen[r.Status] || r.Description == "" {
			fail("Meta.Responses: %d must be a described 2xx status other than %d and the others", r.Status, def)
		}
		seen[r.Status] = true
	}

	ext := make(map[string]any, len(o.Extensions)+6)
	maps.Copy(ext, o.Extensions)
	ext[ExtAction] = m.Action
	ext[ExtSurface] = string(m.Surface)
	ext[ExtRateLimit] = string(rl)
	if m.Idempotency != "" {
		ext[ExtIdempotency] = string(m.Idempotency)
	}
	if m.Reauth {
		ext[ExtReauth] = true
	}
	if m.BreakGlass {
		ext[ExtBreakGlass] = true
		o.Description = withBreakGlassNotice(o.Description)
	}
	o.Extensions = ext
	return o
}

// BreakGlassNotice ends the description of every break-glass operation: the
// justification is not internal (T-326, BE-091), so whoever types it knows
// the user it concerns will read it.
const BreakGlassNotice = "When the read concerns one user, the X-Koro-Access-Reason justification is shown to that user " +
	"(support access notification; reason of the break_glass item of listMySecurityEvents): write no other person's data in it."

// withBreakGlassNotice appends BreakGlassNotice to desc.
func withBreakGlassNotice(desc string) string {
	if desc == "" {
		return BreakGlassNotice
	}
	return desc + " " + BreakGlassNotice
}
