package httpx

import (
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/problem"
)

// The 429 response (BE-178). Every operation can answer 429 rate_limited:
// the global per-IP limit runs before all of them, the operation's own
// class (x-koro-rate-limit) after authentication, and some services apply
// further limits of their own (identity's e-mail, re-authentication and
// SMS send limits). Each of these answers with Retry-After (delay-seconds:
// the limiter's own remaining wait, problem.RetryAfterSeconds), so the
// document states it once, as a reusable response every operation refers
// to. A 429 that is not a matter of waiting (an account locked after
// failed sign-ins, the SMS code cap of one sign-in challenge) carries no
// Retry-After: the header is optional in the document.

// ResponseRateLimited names the reusable 429 response in
// components.responses.
const ResponseRateLimited = "RateLimited"

// HeaderRetryAfter is the response header of a rate limited request.
const HeaderRetryAfter = "Retry-After"

// rateLimitedResponse is the components entry: the problem body
// (schemaRef: the Problem schema) and the Retry-After header.
func rateLimitedResponse(schemaRef *huma.Schema) *huma.Response {
	one := 1.0
	return &huma.Response{
		Description: "Rate limit exceeded (429 rate_limited)",
		Headers: map[string]*huma.Param{
			HeaderRetryAfter: {
				Description: "Seconds to wait before trying again (the limiter's remaining wait, rounded up). Absent " +
					"when waiting does not help (a locked account, the SMS code cap of a sign-in challenge).",
				Schema: &huma.Schema{Type: huma.TypeInteger, Minimum: &one},
			},
		},
		Content: map[string]*huma.MediaType{problem.ContentType: {Schema: schemaRef}},
	}
}

// installRateLimitedResponse adds the 429 response to every operation as
// it is added to oapi (huma's OnAddOperation); an operation that documents
// its own 429 keeps it.
func installRateLimitedResponse(oapi *huma.OpenAPI) {
	status := strconv.Itoa(http.StatusTooManyRequests)
	oapi.OnAddOperation = append(oapi.OnAddOperation, func(_ *huma.OpenAPI, op *huma.Operation) {
		if op.Responses == nil {
			op.Responses = map[string]*huma.Response{}
		}
		if _, own := op.Responses[status]; !own {
			op.Responses[status] = &huma.Response{Ref: "#/components/responses/" + ResponseRateLimited}
		}
	})
}
