// Package official is the İYS adapter over İYS's own API
// (https://apidocs.iys.org.tr, openapi-v1.json 3.2.3; BE-055, BE-105).
//
// Live use needs direct API access: a service provider with fewer than
// 250 000 addresses reaches İYS only through an authorised integrator
// (İYS's İLETİ sales contract), so this adapter serves İLETİ+ or a future
// direct access; the pilot uses an integrator's adapter (BE-110). The
// sandbox is https://api.sandbox.iys.org.tr, live https://api.iys.org.tr;
// İYS accepts registered egress IPs only.
//
//	POST {base}/oauth2/token                                           password / refresh_token grant
//	POST {base}/sps/{iysCode}/brands/{brandCode}/consents              Submit
//	GET  {base}/sps/{iysCode}/brands/{brandCode}/consents/changes      Changes (source=IYS, limit=1000, after)
//	POST {base}/sps/{iysCode}/brands/{brandCode}/consents/{rt}/status/{type}  Statuses: the approvals of ≤ 1000 addresses
//	POST {base}/sps/{iysCode}/brands/{brandCode}/consents/status       Statuses: one address (status, date, source)
//
// Rules:
//
//   - The access token (two hours) is cached and renewed with the refresh
//     token (four hours) before a new password sign-in; sign-ins are
//     serialised and limited to SignInsPerHour (İYS: 10–100 per IP and hour,
//     H084, and a retry within the hour restarts the count), and a 429 of
//     the token endpoint stops sign-ins for an hour.
//   - A 401 renews the token once. A refused sign-in is transient (a
//     configuration problem the next run may not have).
//   - H174 (refusal already held) and H175 (refusal of a recipient İYS has
//     no record of) of a refusal are successes (Receipt.AlreadyHeld); H467
//     is iys.ErrTooLate; other 400/403/404/409/422 answers are
//     iys.ErrRejected; 429 is iys.ErrThrottled; 5xx and transport errors
//     are transient.
//   - Statuses asks the bulk query first (approvals only, no dates) and the
//     single query (status, date, source; IP limit 1000 per hour, H086) only
//     for the addresses the bulk query did not list, within
//     StatusQueriesPerHour; a 429 H086 stops single queries for an hour.
//     A single query of an address İYS has no record of is assumed to
//     answer 404 (undocumented; the sandbox decides).
//   - Changes keeps the configured recipient type and the e-mail, SMS and
//     call types; the last hour of the list is hidden by İYS, so a page
//     that ends the list is complete through its request time minus one
//     hour (and a margin for clock skew). citizenName is never read.
//   - Every error carries İYS's codes and the answer's X-Trace-Id (İYS
//     support asks for it), never the address, a credential or a token;
//     İYS's error texts are scrubbed (obs.Scrub).
package official

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/iys"
	"koro/internal/platform/obs"
)

const (
	dateLayout     = "2006-01-02 15:04:05"
	tokenMargin    = time.Minute
	defaultExpiry  = 2 * time.Hour
	maxBody        = 4 << 20 // a change page of 1000 records
	maxErrorText   = 300
	pageLimit      = 1000
	changeLag      = time.Hour       // İYS hides the last hour of the change list
	clockSkew      = 5 * time.Minute // between İYS's clock and ours
	feedWindow     = 7 * 24 * time.Hour
	windowMargin   = 2 * time.Hour
	blockAfter429  = time.Hour
	defaultStatusQ = 500
	defaultSignIns = 8
)

// İYS recipient types.
const (
	RecipientIndividual = "BIREYSEL"
	RecipientMerchant   = "TACIR"
)

// errSignInRefused: İYS refused the credentials or the refresh token (not
// iys.ErrRejected: the next run may have a fixed configuration).
var errSignInRefused = errors.New("iys official: sign-in refused")

var (
	reCode  = regexp.MustCompile(`^[0-9]{1,12}$`)
	reTrace = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)
	reHCode = regexp.MustCompile(`^H[0-9]{1,5}$`)
)

// Recipients resolves users and İYS recipient addresses in both
// directions (identity, bound in cmd/koro/wire_consent.go); the address is
// used for the request only. iys.ErrRecipientGone when there is none.
type Recipients interface {
	// Recipient returns the address of user for type t.
	Recipient(ctx context.Context, user uuid.UUID, t iys.Type) (string, error)
	// User returns the user holding address for type t.
	User(ctx context.Context, t iys.Type, address string) (uuid.UUID, error)
}

// Config configures the adapter.
type Config struct {
	BaseURL       string // https://api.iys.org.tr (a path prefix is kept)
	Username      string
	Password      string
	IYSCode       string // the service provider's İYS number
	BrandCode     string // the brand's İYS number
	RecipientType string // BIREYSEL (default) | TACIR
	Recipients    Recipients
	Clock         clock.Clock
	// Log receives one debug line per call (operation, HTTP status, trace
	// ID); nil discards.
	Log     *slog.Logger
	Timeout time.Duration // per request; default 10s (İYS: 60s)
	// StatusQueriesPerHour bounds the single status queries (default 500 of
	// İYS's 1000 per IP and hour).
	StatusQueriesPerHour int
	// SignInsPerHour bounds the token requests (default 8 of İYS's 10).
	SignInsPerHour int
	// HTTPClient replaces the client built from Timeout (tests).
	HTTPClient *http.Client
}

// Client is the adapter: Submit, the change feed (Feed, Changes) and single
// states (Statuses).
type Client struct {
	c      Config
	base   string
	brand  string
	client *http.Client

	signMu sync.Mutex // one sign-in at a time

	mu            sync.Mutex
	token         string
	expiry        time.Time
	refresh       string
	refreshExpiry time.Time
	signIns       []time.Time
	signInBlocked time.Time
	statusQueries []time.Time
	statusBlocked time.Time
}

// New validates c.
func New(c Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("iys official: CONSENT_IYS_BASE_URL must be an http(s) URL without credentials or query")
	}
	switch {
	case c.Username == "" || c.Password == "":
		return nil, errors.New("iys official: CONSENT_IYS_USERNAME and CONSENT_IYS_PASSWORD are required")
	case !reCode.MatchString(c.IYSCode) || !reCode.MatchString(c.BrandCode):
		return nil, errors.New("iys official: CONSENT_IYS_CODE and CONSENT_IYS_BRAND_CODE must be İYS numbers")
	case c.Recipients == nil || c.Clock == nil:
		return nil, errors.New("iys official: the adapter needs Recipients and Clock")
	case c.StatusQueriesPerHour < 0 || c.StatusQueriesPerHour > 1000 || c.SignInsPerHour < 0 || c.SignInsPerHour > 100:
		return nil, errors.New("iys official: the hourly budgets must stay within İYS's limits")
	}
	switch c.RecipientType {
	case "":
		c.RecipientType = RecipientIndividual
	case RecipientIndividual, RecipientMerchant:
	default:
		return nil, errors.New("iys official: CONSENT_IYS_RECIPIENT_TYPE must be BIREYSEL or TACIR")
	}
	if c.StatusQueriesPerHour == 0 {
		c.StatusQueriesPerHour = defaultStatusQ
	}
	if c.SignInsPerHour == 0 {
		c.SignInsPerHour = defaultSignIns
	}
	if c.Log == nil {
		c.Log = obs.Discard()
	}
	client := c.HTTPClient
	if client == nil {
		timeout := c.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		client = &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Client{c: c, base: base, brand: "/sps/" + c.IYSCode + "/brands/" + c.BrandCode, client: client}, nil
}

// Name names the adapter in logs.
func (*Client) Name() string { return "iys" }

// Feed describes the change feed: one per brand; İYS's change list keeps
// seven days (less a margin for late runs and clock skew).
func (h *Client) Feed() iys.Feed {
	return iys.Feed{Name: "iys:" + h.c.IYSCode + "/" + h.c.BrandCode, Window: feedWindow - windowMargin}
}

type consentBody struct {
	ConsentDate   string `json:"consentDate"`
	Source        string `json:"source"`
	Recipient     string `json:"recipient"`
	RecipientType string `json:"recipientType"`
	Status        string `json:"status"`
	Type          string `json:"type"`
}

// Submit registers one consent change.
func (h *Client) Submit(ctx context.Context, c iys.Consent) (iys.Receipt, error) {
	switch {
	case !iys.ValidType(c.Type):
		return iys.Receipt{}, fmt.Errorf("%w: unknown İYS type", iys.ErrRejected)
	case c.Status != iys.Approved && c.Status != iys.Refused:
		return iys.Receipt{}, fmt.Errorf("%w: unknown İYS status", iys.ErrRejected)
	case iys.PlainSource(c.Source) == "" || c.ConsentDate.IsZero():
		return iys.Receipt{}, fmt.Errorf("%w: consent without source or date", iys.ErrRejected)
	}
	recipient, err := h.c.Recipients.Recipient(ctx, c.UserID, c.Type)
	switch {
	case errors.Is(err, iys.ErrRecipientGone) || (err == nil && recipient == ""):
		return iys.Receipt{}, fmt.Errorf("%w: %w", iys.ErrRejected, iys.ErrRecipientGone)
	case err != nil:
		return iys.Receipt{}, fmt.Errorf("iys official: recipient: %w", err)
	}
	body, err := json.Marshal(consentBody{
		ConsentDate: c.ConsentDate.In(clock.Istanbul()).Format(dateLayout), Source: c.Source, Recipient: recipient,
		RecipientType: h.c.RecipientType, Status: string(c.Status), Type: string(c.Type),
	})
	if err != nil {
		return iys.Receipt{}, fmt.Errorf("iys official: encode: %w", err)
	}
	var out struct {
		TransactionID string `json:"transactionId"`
	}
	_, err = h.call(ctx, "submit", http.MethodPost, h.brand+"/consents", nil, body, &out)
	var ae *apiError
	if errors.As(err, &ae) && c.Status == iys.Refused && (ae.has("H174") || ae.has("H175")) {
		code := "H174" // İYS holds the refusal already
		if ae.has("H175") {
			code = "H175" // İYS has no record to refuse
		}
		h.c.Log.LogAttrs(ctx, slog.LevelDebug, "iys refusal already held", slog.String("code", code),
			slog.String("trace_id", ae.trace), slog.String("ref", cutText(c.Ref, 64)))
		return iys.Receipt{AlreadyHeld: true}, nil
	}
	if err != nil {
		return iys.Receipt{}, err
	}
	return iys.Receipt{TransactionID: cutText(out.TransactionID, 128)}, nil
}

type changeItem struct {
	ConsentDate   string `json:"consentDate"`
	CreationDate  string `json:"creationDate"`
	Source        string `json:"source"`
	Recipient     string `json:"recipient"`
	RecipientType string `json:"recipientType"`
	Status        string `json:"status"`
	Type          string `json:"type"`
	// citizenName (IYS_WEB_YENI) is personal data and never decoded.
}

// Changes returns the next page of İYS's change list (source=IYS) after
// cursor.
func (h *Client) Changes(ctx context.Context, cursor string) (iys.ChangePage, error) {
	if !iys.ValidCursor(cursor) {
		return iys.ChangePage{}, fmt.Errorf("%w: malformed cursor", iys.ErrRejected)
	}
	q := url.Values{"source": {"IYS"}, "limit": {strconv.Itoa(pageLimit)}}
	if cursor != "" {
		q.Set("after", cursor)
	}
	asked := h.c.Clock.Now()
	var out struct {
		After string       `json:"after"`
		List  []changeItem `json:"list"`
	}
	status, err := h.call(ctx, "changes", http.MethodGet, h.brand+"/consents/changes", q, nil, &out)
	if err != nil {
		return iys.ChangePage{}, err
	}
	page := iys.ChangePage{Cursor: cursor, Through: asked.Add(-changeLag - clockSkew)}
	if status == http.StatusNoContent || len(out.List) == 0 {
		return page, nil
	}
	if out.After == "" || !iys.ValidCursor(out.After) {
		return iys.ChangePage{}, errors.New("iys official: changes: malformed answer (after)")
	}
	page.Cursor, page.More = out.After, len(out.List) >= pageLimit
	var last time.Time
	for _, it := range out.List {
		created, err := parseDate(it.CreationDate)
		if err != nil {
			return iys.ChangePage{}, errors.New("iys official: changes: malformed answer (creationDate)")
		}
		last = created
		typ := iys.Type(it.Type)
		if !iys.ValidType(typ) || def(it.RecipientType, RecipientIndividual) != h.c.RecipientType {
			page.Skipped++
			continue
		}
		st := iys.State{Status: iys.Status(it.Status), Source: iys.PlainSource(it.Source)}
		if st.Status != iys.Approved && st.Status != iys.Refused {
			return iys.ChangePage{}, errors.New("iys official: changes: malformed answer (status)")
		}
		if st.At, err = parseDate(it.ConsentDate); err != nil {
			st.At = created // a merchant's first record may come without a consent date
		}
		user, err := h.c.Recipients.User(ctx, typ, it.Recipient)
		switch {
		case errors.Is(err, iys.ErrRecipientGone):
			page.Skipped++
			continue
		case err != nil:
			return iys.ChangePage{}, fmt.Errorf("iys official: changes: recipient: %w", err)
		}
		page.Changes = append(page.Changes, iys.Change{UserID: user, Type: typ, State: st, RecordedAt: created})
	}
	if page.More {
		page.Through = last
	}
	return page, nil
}

// Statuses returns İYS's current state of the users' addresses for t.
func (h *Client) Statuses(ctx context.Context, t iys.Type, userIDs []uuid.UUID) (map[uuid.UUID]iys.State, error) {
	switch {
	case !iys.ValidType(t):
		return nil, fmt.Errorf("%w: unknown İYS type", iys.ErrRejected)
	case len(userIDs) > iys.MaxStatusBatch:
		return nil, fmt.Errorf("iys official: %d users in one status read (at most %d)", len(userIDs), iys.MaxStatusBatch)
	}
	users := map[string]uuid.UUID{}
	var addresses []string
	for _, id := range userIDs {
		a, err := h.c.Recipients.Recipient(ctx, id, t)
		switch {
		case errors.Is(err, iys.ErrRecipientGone) || (err == nil && a == ""):
			continue
		case err != nil:
			return nil, fmt.Errorf("iys official: recipient: %w", err)
		}
		k := normAddress(t, a)
		if _, dup := users[k]; !dup {
			users[k] = id
			addresses = append(addresses, a)
		}
	}
	out := make(map[uuid.UUID]iys.State, len(addresses))
	if len(addresses) == 0 {
		return out, nil
	}
	body, err := json.Marshal(map[string][]string{"recipients": addresses})
	if err != nil {
		return nil, fmt.Errorf("iys official: encode: %w", err)
	}
	var bulk struct {
		List []string `json:"list"`
	}
	if _, err := h.call(ctx, "bulk status", http.MethodPost, h.brand+"/consents/"+h.c.RecipientType+"/status/"+string(t), nil, body, &bulk); err != nil {
		return nil, err
	}
	for _, a := range bulk.List {
		if id, ok := users[normAddress(t, a)]; ok {
			out[id] = iys.State{Status: iys.Approved}
		}
	}
	for _, a := range addresses {
		id := users[normAddress(t, a)]
		if _, ok := out[id]; ok {
			continue
		}
		st, found, err := h.single(ctx, t, a)
		if err != nil {
			return nil, err
		}
		if found {
			out[id] = st
		}
	}
	return out, nil
}

// single is the single status query of one address.
func (h *Client) single(ctx context.Context, t iys.Type, address string) (iys.State, bool, error) {
	if err := h.takeStatusQuery(); err != nil {
		return iys.State{}, false, err
	}
	body, err := json.Marshal(map[string]string{"recipient": address, "recipientType": h.c.RecipientType, "type": string(t)})
	if err != nil {
		return iys.State{}, false, fmt.Errorf("iys official: encode: %w", err)
	}
	var it changeItem
	_, err = h.call(ctx, "status", http.MethodPost, h.brand+"/consents/status", nil, body, &it)
	var ae *apiError
	switch {
	case errors.As(err, &ae) && ae.status == http.StatusNotFound:
		return iys.State{}, false, nil // no record (assumed answer; T-354)
	case errors.As(err, &ae) && ae.has("H086"):
		h.mu.Lock()
		h.statusBlocked = h.c.Clock.Now().Add(blockAfter429)
		h.mu.Unlock()
		return iys.State{}, false, err
	case err != nil:
		return iys.State{}, false, err
	case it.Status == "":
		return iys.State{}, false, nil
	case it.Status != string(iys.Approved) && it.Status != string(iys.Refused):
		return iys.State{}, false, errors.New("iys official: status: malformed answer (status)")
	}
	st := iys.State{Status: iys.Status(it.Status), Source: iys.PlainSource(it.Source)}
	if st.At, err = parseDate(it.ConsentDate); err != nil {
		if st.At, err = parseDate(it.CreationDate); err != nil {
			return iys.State{}, false, errors.New("iys official: status: malformed answer (consentDate)")
		}
	}
	return st, true, nil
}

// takeStatusQuery spends one single status query of the hourly budget.
func (h *Client) takeStatusQuery() error {
	now := h.c.Clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statusQueries = lastHour(h.statusQueries, now)
	switch {
	case now.Before(h.statusBlocked):
		return fmt.Errorf("%w: İYS single status queries paused until %s (H086)", iys.ErrThrottled, h.statusBlocked.UTC().Format(time.RFC3339))
	case len(h.statusQueries) >= h.c.StatusQueriesPerHour:
		return fmt.Errorf("%w: single status query budget of %d per hour spent", iys.ErrThrottled, h.c.StatusQueriesPerHour)
	}
	h.statusQueries = append(h.statusQueries, now)
	return nil
}

// call sends one authorised request; a 401 renews the token once.
func (h *Client) call(ctx context.Context, op, method, path string, q url.Values, body []byte, out any) (int, error) {
	stale := ""
	for attempt := 0; ; attempt++ {
		tok, err := h.accessToken(ctx, stale)
		if err != nil {
			return 0, err
		}
		status, err := h.send(ctx, op, method, path, q, "Bearer "+tok, body, out)
		if status == http.StatusUnauthorized && attempt == 0 {
			stale = tok
			continue
		}
		return status, err
	}
}

// accessToken returns the cached token unless it is stale (the one a 401
// answered), renewing with the refresh token or signing in again.
func (h *Client) accessToken(ctx context.Context, stale string) (string, error) {
	if tok, ok := h.cached(stale); ok {
		return tok, nil
	}
	h.signMu.Lock()
	defer h.signMu.Unlock()
	if tok, ok := h.cached(stale); ok { // another caller renewed it meanwhile
		return tok, nil
	}
	now := h.c.Clock.Now()
	h.mu.Lock()
	h.signIns = lastHour(h.signIns, now)
	blocked, spent, refresh := now.Before(h.signInBlocked), len(h.signIns) >= h.c.SignInsPerHour, ""
	if now.Before(h.refreshExpiry.Add(-tokenMargin)) {
		refresh = h.refresh
	}
	h.mu.Unlock()
	switch {
	case blocked:
		return "", fmt.Errorf("%w: İYS sign-in paused after a 429 (H084)", iys.ErrThrottled)
	case spent:
		return "", fmt.Errorf("%w: İYS sign-in budget of %d per hour spent", iys.ErrThrottled, h.c.SignInsPerHour)
	}
	if refresh != "" {
		tok, err := h.signIn(ctx, map[string]string{"grant_type": "refresh_token", "refresh_token": refresh})
		if !errors.Is(err, errSignInRefused) { // success, throttled or İYS unreachable: no password attempt
			return tok, err
		}
		h.mu.Lock()
		h.refresh, h.signIns = "", lastHour(h.signIns, h.c.Clock.Now())
		spent = len(h.signIns) >= h.c.SignInsPerHour
		h.mu.Unlock()
		if spent {
			return "", fmt.Errorf("%w: İYS sign-in budget of %d per hour spent", iys.ErrThrottled, h.c.SignInsPerHour)
		}
	}
	return h.signIn(ctx, map[string]string{"grant_type": "password", "username": h.c.Username, "password": h.c.Password})
}

func (h *Client) cached(stale string) (string, bool) {
	now := h.c.Clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.token != "" && h.token != stale && now.Before(h.expiry.Add(-tokenMargin)) {
		return h.token, true
	}
	return "", false
}

// signIn runs one token request (call with signMu held).
func (h *Client) signIn(ctx context.Context, grant map[string]string) (string, error) {
	body, err := json.Marshal(grant)
	if err != nil {
		return "", err
	}
	defer clear(body)
	now := h.c.Clock.Now()
	h.mu.Lock()
	h.signIns = append(h.signIns, now)
	h.mu.Unlock()
	var out struct {
		AccessToken      string `json:"access_token"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshToken     string `json:"refresh_token"`
		RefreshExpiresIn int64  `json:"refresh_expires_in"`
	}
	status, err := h.send(ctx, "sign-in", http.MethodPost, "/oauth2/token", nil, "", body, &out)
	switch {
	case status == http.StatusTooManyRequests:
		h.mu.Lock()
		h.signInBlocked = now.Add(blockAfter429)
		h.mu.Unlock()
		return "", err
	case errors.Is(err, iys.ErrRejected): // a configuration problem: transient, retried by the next run
		return "", fmt.Errorf("%w: %s", errSignInRefused, strings.TrimPrefix(err.Error(), "iys official: sign-in: "))
	case err != nil:
		return "", err
	case out.AccessToken == "":
		return "", errors.New("iys official: sign-in: no access token in the answer")
	}
	exp := time.Duration(out.ExpiresIn) * time.Second
	if exp <= 0 {
		exp = defaultExpiry
	}
	h.mu.Lock()
	h.token, h.expiry = out.AccessToken, now.Add(exp)
	h.refresh, h.refreshExpiry = out.RefreshToken, now.Add(time.Duration(out.RefreshExpiresIn)*time.Second)
	h.mu.Unlock()
	return out.AccessToken, nil
}

// apiError is a non-2xx answer: HTTP status, İYS's codes, the scrubbed
// text and the trace ID. It unwraps to the port error of its class.
type apiError struct {
	op     string
	status int
	codes  []string
	text   string
	trace  string
	class  []error // the port errors it wraps
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("iys official: %s: HTTP %d: %s", e.op, e.status, e.text)
	if e.trace != "" {
		s += " (trace " + e.trace + ")"
	}
	return s
}

func (e *apiError) Unwrap() []error { return e.class }

func (e *apiError) has(code string) bool { return slices.Contains(e.codes, code) }

// send runs one request. Errors carry the operation, the status, İYS's
// scrubbed codes and texts and the trace ID, never the request.
func (h *Client) send(ctx context.Context, op, method, path string, q url.Values, auth string, body []byte, out any) (int, error) {
	u := h.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, fmt.Errorf("iys official: %s: cannot build the request", op)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, fmt.Errorf("iys official: %s: %s", op, text(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	defer clear(raw) // may hold addresses
	trace := resp.Header.Get("X-Trace-Id")
	if !reTrace.MatchString(trace) {
		trace = ""
	}
	h.c.Log.LogAttrs(ctx, slog.LevelDebug, "iys call", slog.String("op", op), slog.Int("status", resp.StatusCode),
		slog.String("trace_id", trace))
	if resp.StatusCode/100 == 2 {
		if out != nil && resp.StatusCode != http.StatusNoContent && len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, out) != nil {
			return resp.StatusCode, fmt.Errorf("iys official: %s: malformed answer (trace %s)", op, trace)
		}
		return resp.StatusCode, nil
	}
	ae := &apiError{op: op, status: resp.StatusCode, trace: trace}
	ae.codes, ae.text = errorsOf(raw)
	switch {
	case ae.has("H467"):
		ae.class = []error{iys.ErrRejected, iys.ErrTooLate}
	case resp.StatusCode == http.StatusTooManyRequests:
		ae.class = []error{iys.ErrThrottled}
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusConflict, resp.StatusCode == http.StatusUnprocessableEntity:
		ae.class = []error{iys.ErrRejected}
	}
	return resp.StatusCode, ae
}

// errorsOf extracts İYS's error codes and the scrubbed texts of an error
// answer ({"errors":[{"code":"H174","message":"…"}]} or {"message":"…"}).
func errorsOf(raw []byte) ([]string, string) {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &e)
	var codes, parts []string
	for _, x := range e.Errors {
		code := ""
		if reHCode.MatchString(x.Code) {
			code = x.Code
			codes = append(codes, code)
		}
		parts = append(parts, strings.TrimSpace(code+" "+x.Message))
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if len(parts) == 0 {
		return codes, "no error text"
	}
	return codes, text(strings.Join(parts, "; "))
}

// text scrubs addresses and control characters and bounds the length.
func text(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, obs.Scrub(s))
	return cutText(s, maxErrorText)
}

func cutText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func parseDate(s string) (time.Time, error) {
	t, err := time.ParseInLocation(dateLayout, s, clock.Istanbul())
	return t.UTC(), err
}

// normAddress is the form addresses are compared in: İYS keeps e-mail
// addresses in lower case.
func normAddress(t iys.Type, a string) string {
	if t == iys.Email {
		return strings.ToLower(strings.TrimSpace(a))
	}
	return strings.TrimSpace(a)
}

func def(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func lastHour(ts []time.Time, now time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < time.Hour {
			out = append(out, t)
		}
	}
	return out
}
