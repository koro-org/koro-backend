package problem

import "net/http"

// Generic sentinels that need no module code (05 §4.2).
var (
	ErrUnauthenticated      = New(Unauthenticated, "unauthenticated", "Authentication required")
	ErrForbidden            = New(Forbidden, "forbidden", "Forbidden")
	ErrNotFound             = New(NotFound, "not_found", "Not found")
	ErrConflict             = New(Conflict, "conflict", "Conflict")
	ErrGone                 = New(Gone, "gone", "Gone")
	ErrPreconditionFailed   = New(PreconditionFailed, "precondition_failed", "Precondition failed")
	ErrPreconditionRequired = New(PreconditionRequired, "precondition_required", "Precondition required")
	ErrRateLimited          = New(RateLimited, "rate_limited", "Rate limit exceeded")
	ErrUnavailable          = New(Unavailable, "unavailable", "Service unavailable")
	ErrInternal             = New(Internal, "internal_error", "Internal server error")
	ErrValidation           = New(Invalid, "validation_failed", "Validation failed")
)

// Transport-level codes produced by the HTTP layer itself (router, body
// limits, content negotiation, CSRF). They have no Kind because services
// never return them.
const (
	CodeBadRequest           = "bad_request"
	CodeMethodNotAllowed     = "method_not_allowed"
	CodeNotAcceptable        = "not_acceptable"
	CodeRequestTimeout       = "request_timeout"
	CodePayloadTooLarge      = "payload_too_large"
	CodeUnsupportedMediaType = "unsupported_media_type"
	CodeCSRFRejected         = "csrf_rejected"
)

// Catalog registrations of the transport codes (package-level initialisation,
// no init function).
var _ = []bool{
	register(CodeBadRequest, http.StatusBadRequest, "Bad request"),
	register(CodeMethodNotAllowed, http.StatusMethodNotAllowed, "Method not allowed"),
	register(CodeNotAcceptable, http.StatusNotAcceptable, "Not acceptable"),
	register(CodeRequestTimeout, http.StatusRequestTimeout, "Request timeout"),
	register(CodePayloadTooLarge, http.StatusRequestEntityTooLarge, "Payload too large"),
	register(CodeUnsupportedMediaType, http.StatusUnsupportedMediaType, "Unsupported media type"),
	register(CodeCSRFRejected, http.StatusForbidden, "Cross-origin request rejected"),
}

// CodeForStatus returns the generic code used when only an HTTP status is
// known (huma validation/parsing errors, router errors).
func CodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return CodeBadRequest
	case http.StatusUnauthorized:
		return ErrUnauthenticated.code
	case http.StatusForbidden:
		return ErrForbidden.code
	case http.StatusNotFound:
		return ErrNotFound.code
	case http.StatusMethodNotAllowed:
		return CodeMethodNotAllowed
	case http.StatusNotAcceptable:
		return CodeNotAcceptable
	case http.StatusRequestTimeout:
		return CodeRequestTimeout
	case http.StatusConflict:
		return ErrConflict.code
	case http.StatusGone:
		return ErrGone.code
	case http.StatusPreconditionFailed:
		return ErrPreconditionFailed.code
	case http.StatusRequestEntityTooLarge:
		return CodePayloadTooLarge
	case http.StatusUnsupportedMediaType:
		return CodeUnsupportedMediaType
	case http.StatusUnprocessableEntity:
		return ErrValidation.code
	case http.StatusPreconditionRequired:
		return ErrPreconditionRequired.code
	case http.StatusTooManyRequests:
		return ErrRateLimited.code
	case http.StatusServiceUnavailable:
		return ErrUnavailable.code
	}
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return CodeBadRequest
	}
	return ErrInternal.code
}
