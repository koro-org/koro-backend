package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"koro/internal/platform/money"
)

// The plumbing of ParasutProvider: JSON:API documents, one authorised
// call with the 401 renewal, the error classes and the PDF download.

// ---- JSON:API documents

// jsonAPIResource is a resource object as Paraşüt answers it.
type jsonAPIResource struct {
	ID            string                    `json:"id"`
	Type          string                    `json:"type"`
	Attributes    json.RawMessage           `json:"attributes"`
	Relationships map[string]jsonAPIRelRead `json:"relationships"`
}

// jsonAPIRelRead is a relationship of an answer (an object, an array or null).
type jsonAPIRelRead struct {
	Data json.RawMessage `json:"data"`
}

func (r jsonAPIResource) attrs(v any) error {
	if len(r.Attributes) == 0 {
		return errors.New("no attributes")
	}
	return json.Unmarshal(r.Attributes, v)
}

// rel returns the to-one relationship name ({type, id}).
func (r jsonAPIResource) rel(name string) (jsonAPIRef, bool) {
	d, ok := r.Relationships[name]
	if !ok || len(d.Data) == 0 {
		return jsonAPIRef{}, false
	}
	var ref jsonAPIRef
	if json.Unmarshal(d.Data, &ref) != nil || ref.ID == "" {
		return jsonAPIRef{}, false
	}
	return ref, true
}

// jsonAPIOne is a single-resource document with its includes.
type jsonAPIOne struct {
	Data     jsonAPIResource   `json:"data"`
	Included []jsonAPIResource `json:"included"`
}

func (o jsonAPIOne) included(ref jsonAPIRef) (jsonAPIResource, bool) {
	for _, r := range o.Included {
		if r.Type == ref.Type && r.ID == ref.ID {
			return r, true
		}
	}
	return jsonAPIResource{}, false
}

// jsonAPIList is a collection document.
type jsonAPIList struct {
	Data []jsonAPIResource `json:"data"`
}

// jsonAPIBody is a request document.
type jsonAPIBody struct {
	Data jsonAPINew `json:"data"`
}

// jsonAPINew is a resource to create (no ID).
type jsonAPINew struct {
	Type          string                `json:"type"`
	Attributes    any                   `json:"attributes,omitempty"`
	Relationships map[string]jsonAPIRel `json:"relationships,omitempty"`
}

// jsonAPIRel is a relationship of a request.
type jsonAPIRel struct {
	Data any `json:"data"`
}

// jsonAPIRef is a resource identifier.
type jsonAPIRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// decimal is a decimal number sent as a JSON number (money.Decimal output).
type decimal string

var reDecimal = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// MarshalJSON implements json.Marshaler.
func (d decimal) MarshalJSON() ([]byte, error) {
	if !reDecimal.MatchString(string(d)) {
		return nil, fmt.Errorf("billing parasut: %q is not a decimal number", string(d))
	}
	return []byte(d), nil
}

// jsonNumber reads a number Paraşüt sends as a JSON number or a string
// ("" for null).
type jsonNumber string

// UnmarshalJSON implements json.Unmarshaler.
func (n *jsonNumber) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || bytes.Equal(b, []byte("null")):
		*n = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*n = jsonNumber(strings.TrimSpace(s))
	default:
		*n = jsonNumber(b)
	}
	return nil
}

// kurus parses an amount into kuruş: zeros beyond the second decimal are
// dropped, any other precision (a float artefact) or a non-number is false.
func (n jsonNumber) kurus() (int64, bool) {
	s := string(n)
	if whole, frac, ok := strings.Cut(s, "."); ok && len(frac) > 2 {
		if strings.TrimRight(frac[2:], "0") != "" {
			return 0, false
		}
		s = whole + "." + frac[:2]
	}
	v, err := money.ParseDecimal(s)
	return v, err == nil
}

// ---- calls

// call sends one authorised request under /v4/{company}; a 401 renews the
// token once and repeats the call once.
func (c *ParasutProvider) call(ctx context.Context, op, method, path string, q url.Values, body, out any) (int, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return 0, fmt.Errorf("billing parasut: %s: encode: %w", op, err)
		}
		defer clear(raw) // holds the parties' data
	}
	stale := ""
	for attempt := 0; ; attempt++ {
		var (
			tok ProviderToken
			err error
		)
		if stale == "" {
			tok, err = c.tokens.Token(ctx)
		} else {
			tok, err = c.tokens.Refresh(ctx, stale)
		}
		switch {
		case err != nil:
			return 0, err
		case tok.Access == "":
			return 0, fmt.Errorf("%w: no Paraşüt access token", ErrNoCredentials)
		case !reCompany.MatchString(tok.Account):
			return 0, fmt.Errorf("%w: the Paraşüt company ID (external_account) must be a number", ErrNoCredentials)
		}
		status, err := c.send(ctx, op, method, "/v4/"+tok.Account+path, q, tok.Access, raw, out)
		if status == http.StatusUnauthorized && attempt == 0 {
			stale = tok.Access
			continue
		}
		return status, err
	}
}

// reCompany is Paraşüt's company ID.
var reCompany = regexp.MustCompile(`^[0-9]{1,20}$`)

// send runs one request. Its errors carry the operation, the status and
// Paraşüt's scrubbed error titles, never the request.
func (c *ParasutProvider) send(ctx context.Context, op, method, path string, q url.Values, token string, body []byte, out any) (int, error) {
	if c.limiter != nil {
		if res, err := c.limiter.Allow(ctx, c.rule, "parasut"); err == nil && !res.Allowed {
			return 0, &apiError{op: op, text: fmt.Sprintf("call budget of %d per 10 seconds spent", c.rule.Rate)}
		}
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, fmt.Errorf("billing parasut: %s: cannot build the request", op)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.api+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/vnd.api+json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("billing parasut: %s: %s", op, transportError(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	defer clear(raw) // may hold the parties' data
	c.log.LogAttrs(ctx, slog.LevelDebug, "parasut call", slog.String("op", op), slog.Int("status", resp.StatusCode))
	if resp.StatusCode/100 == 2 {
		if out != nil && resp.StatusCode != http.StatusNoContent && len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, out) != nil {
			return resp.StatusCode, fmt.Errorf("billing parasut: %s: malformed answer", op)
		}
		return resp.StatusCode, nil
	}
	ae := &apiError{op: op, status: resp.StatusCode}
	ae.text, ae.existing = errorsOf(raw)
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		ae.class = []error{ErrProviderInvalid}
	case http.StatusNotFound:
		ae.class = []error{ErrProviderNotFound}
	}
	return resp.StatusCode, ae
}

// apiError is a refused call: the operation, the HTTP status and
// Paraşüt's scrubbed error titles; it unwraps to the port error of its
// class (none: transient).
type apiError struct {
	op       string
	status   int
	text     string
	existing string // meta.sales_invoice_id of a duplicate external_reference_key
	class    []error
}

func (e *apiError) Error() string {
	s := "billing parasut: " + e.op
	if e.status != 0 {
		s += fmt.Sprintf(": HTTP %d", e.status)
	}
	switch e.status {
	case http.StatusPaymentRequired:
		s += " (Paraşüt usage period ended)"
	case http.StatusForbidden:
		s += " (role or package does not allow it)"
	case http.StatusTooManyRequests:
		s += " (rate limit)"
	}
	if e.text != "" {
		s += ": " + e.text
	}
	return s
}

func (e *apiError) Unwrap() []error { return e.class }

// errorsOf reads Paraşüt's error document: the titles and pointers (never
// the details, which may repeat the request's data) and the invoice a
// duplicate external_reference_key names. 402 answers carry plain strings;
// an invalid body answers {"error": "…"}.
func errorsOf(raw []byte) (text, existing string) {
	var doc struct {
		Errors []json.RawMessage `json:"errors"`
		Error  string            `json:"error"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", ""
	}
	var parts []string
	for _, e := range doc.Errors {
		var s string
		if json.Unmarshal(e, &s) == nil {
			parts = append(parts, s)
			continue
		}
		var o struct {
			Title  string `json:"title"`
			Code   string `json:"code"`
			Source struct {
				Pointer   string `json:"pointer"`
				Parameter string `json:"parameter"`
			} `json:"source"`
			Meta struct {
				SalesInvoiceID jsonNumber `json:"sales_invoice_id"`
			} `json:"meta"`
		}
		if json.Unmarshal(e, &o) != nil {
			continue
		}
		part := strings.TrimSpace(o.Code + " " + o.Title)
		if where := o.Source.Pointer + o.Source.Parameter; where != "" {
			part += " (" + where + ")"
		}
		parts = append(parts, part)
		if id := string(o.Meta.SalesInvoiceID); existing == "" && reID.MatchString(id) {
			existing = id
		}
	}
	if doc.Error != "" {
		parts = append(parts, doc.Error)
	}
	return safeText(strings.Join(parts, "; ")), existing
}

// download fetches a PDF link of Paraşüt (a short-lived URL of another
// host): never with the bearer token, https only (http only when the API
// itself is http: a local double), at most maxPDF bytes, a PDF.
func (c *ParasutProvider) download(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.User != nil:
		return nil, errors.New("billing parasut: pdf: unusable link")
	case u.Scheme != "https" && (u.Scheme != "http" || !strings.HasPrefix(c.base, "http://")):
		return nil, errors.New("billing parasut: pdf: the link must be https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("billing parasut: pdf: cannot build the request")
	}
	req.Header.Set("Accept", "application/pdf")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("billing parasut: pdf: %s", transportError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("billing parasut: pdf: HTTP %d", resp.StatusCode)
	}
	pdf, err := io.ReadAll(io.LimitReader(resp.Body, maxPDF+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("billing parasut: pdf: %s", transportError(err))
	case len(pdf) > maxPDF:
		return nil, fmt.Errorf("%w: the PDF is larger than %d bytes", ErrProviderInvalid, maxPDF)
	case !bytes.HasPrefix(pdf, []byte("%PDF-")):
		return nil, errors.New("billing parasut: pdf: the link did not answer a PDF")
	}
	return pdf, nil
}
