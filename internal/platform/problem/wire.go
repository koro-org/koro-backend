package problem

import "net/http"

// ContentType is the media type of every error response (RFC 9457).
const ContentType = "application/problem+json"

// Problem is the RFC 9457 response body plus the "code" extension member.
// It satisfies huma's StatusError and ContentTypeFilter interfaces
// structurally, so this package does not depend on huma.
type Problem struct {
	Type     string       `json:"type" format:"uri-reference" doc:"PROBLEM_TYPE_BASE + code" example:"urn:koro:problem:validation_failed"`
	Title    string       `json:"title" doc:"Fixed English summary of the code; clients translate by code" example:"Validation failed"`
	Status   int          `json:"status" doc:"HTTP status code" example:"422"`
	Detail   string       `json:"detail,omitempty" doc:"Occurrence-specific explanation without personal data"`
	Instance string       `json:"instance,omitempty" format:"uri-reference" doc:"urn:koro:request:<request id>" example:"urn:koro:request:01J9Z3T2Q5V6"`
	Code     string       `json:"code" doc:"Stable machine-readable error code" example:"validation_failed"`
	Errors   []FieldError `json:"errors,omitempty" doc:"Field errors (never contain the submitted value)"`
}

// Error returns the title.
func (p *Problem) Error() string { return p.Title }

// GetStatus returns the HTTP status (huma.StatusError).
func (p *Problem) GetStatus() int { return p.Status }

// ContentType maps JSON to application/problem+json (huma.ContentTypeFilter).
func (p *Problem) ContentType(ct string) string {
	if ct == "application/json" {
		return ContentType
	}
	return ct
}

// FromError builds the response body of e. Internal errors carry no
// details beyond their generic title.
func FromError(e *Error, typeBase string) *Problem {
	p := &Problem{
		Type:   typeBase + e.code,
		Title:  e.title,
		Status: e.Status(),
		Code:   e.code,
	}
	if e.kind != Internal && len(e.fields) > 0 {
		p.Errors = e.Fields()
	}
	return p
}

// ForStatus builds a response body when only a status is known. The title
// comes from the catalog entry of the generic code; detail is optional and
// must not contain personal data or internal details.
func ForStatus(status int, detail, typeBase string) *Problem {
	return ForCode(CodeForStatus(status), status, detail, typeBase)
}

// ForCode builds a response body for a catalogued code.
func ForCode(code string, status int, detail, typeBase string) *Problem {
	title := http.StatusText(status)
	if e, ok := lookup(code); ok {
		title = e.Title
	}
	if status >= http.StatusInternalServerError {
		detail = ""
	}
	return &Problem{
		Type:   typeBase + code,
		Title:  title,
		Status: status,
		Detail: detail,
		Code:   code,
	}
}
