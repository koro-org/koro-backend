// Package netgsm is the İYS adapter over the Netgsm integrator's API
// (NetİYS; BE-110). A service provider with fewer than 250 000 addresses
// reaches İYS only through an authorised integrator; the pilot uses Netgsm
// (koro-docs docs/research/iys-entegrator-arastirma.md §9). Sources: Netgsm's API
// documentation (https://www.netgsm.com.tr/dokuman/, "NETIYS", read
// 2026-10-25) and its PHP package netgsm/iys (https://github.com/netgsm1/netiys).
//
//	POST {base}/iys/add     Submit: {"header": {username, password, brandCode[, refid]},
//	                        "body": {"data": [{type, source, recipient, status, consentDate, recipientType}]}}
//	                        → {"code": "0", "error": "false", "uid": "…"[, "erroritem": {…}]}
//	POST {base}/iys/search  Statuses, Recheck: {"header": {…}, "body": {"data": [{type, recipient, recipientType}]}}
//	                        → {"code": "0", "query": {status, type, recipient, recipientType, transactionId, …}};
//	                        code 50: no record
//	webhook (Netgsm → Koro) Receive: [{iyscode, brandcode, submitid, resultstatus, errcode, errmsg,
//	                        creationdate, user_refcode, transactionid, type, source, recipient, status,
//	                        consentDate, recipientType, …}]
//
// Rules:
//
//   - An upload is asynchronous: /iys/add answers with Netgsm's request ID
//     only; İYS's answer (success with its transactionid, or failure with
//     its Hxxx code) arrives by webhook. Submit sends a reference once
//     (again after ResendAfter, or after a transient İYS failure) and waits
//     up to ResultWait for the answer in the store right after sending (a
//     later attempt only looks it up); without one it fails transiently
//     (consent retries), so a submission is "sent" only once İYS took it.
//   - İYS's answer: H174 and H175 of a refusal are successes
//     (Receipt.AlreadyHeld); H467 is iys.ErrTooLate; H103, H470, H412, H015,
//     H124, H084 and H086 (İYS errors and limits) send again; any other code
//     is iys.ErrRejected.
//   - Netgsm's codes: 30 (credentials, API access, IP restriction), 40 (İYS
//     module inactive), 60 (brand code), 70 (parameters), 8010 (too many
//     addresses) and a rejected address (erroritem) are iys.ErrRejected;
//     100 and 101 (system error, at most 10 requests per minute) are
//     iys.ErrThrottled; anything else, HTTP 5xx and transport errors are
//     transient.
//   - Budgets (per process): RequestsPerMinute for every call (Netgsm: 10),
//     StatusQueriesPerHour for the single queries (Statuses, Recheck). A
//     spent budget is iys.ErrThrottled without a call.
//   - The webhook (Receive) carries no signature in Netgsm's documentation:
//     a delivery proves itself by the secret token of its URL (≥ 32
//     characters, compared in constant time) and, when configured, by its
//     source address. Its items are checked against the configured İYS and
//     brand codes; an "IYS_*" source is a change of the feed, an "HS_*" one
//     an answer to an upload. The address is resolved to the Koro user on
//     receipt and dropped; names and texts are never decoded. An address of
//     no Koro user is kept as an anonymous marker (counted as skipped).
//     Items are de-duplicated by digest and kept Retention (seven days;
//     PurgeJob, policy iys_netgsm_event).
//   - Changes reads the stored changes after the cursor (the store's
//     sequence) that arrived at least Settle ago (rows of concurrent
//     deliveries commit in any order); a page that ends the feed is
//     complete through that bound.
//   - Every error carries Netgsm's or İYS's code, never the address, a
//     credential or Netgsm's free text (which repeats the address).
//
// Not in Netgsm's documentation, so configurable or assumed (00 T-368): the
// webhook's authentication, retries and expected answer; the delay of
// İYS's answers (ResultWait, ResendAfter); where refid goes (RefIn) and
// whether every answer echoes it (answers without one are matched by
// user, type, status and consent date); whether the 10 requests per minute
// are per endpoint; whether the search answer carries dates and source.
package netgsm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/iys"
	"koro/internal/platform/obs"
)

// Defaults and bounds.
const (
	DefaultBaseURL              = "https://api.netgsm.com.tr"
	DefaultRequestsPerMinute    = 8   // of Netgsm's 10 per minute
	DefaultStatusQueriesPerHour = 240 // single queries: Statuses and Recheck
	DefaultResultWait           = 15 * time.Second
	DefaultResendAfter          = time.Hour
	// Retention is how long the webhook items are kept at least (the feed's
	// window, like İYS's own change list; retention policy
	// RetentionPolicyCode).
	Retention = 7 * 24 * time.Hour
	// RetentionPolicyCode is the retention policy of the webhook items
	// (000147).
	RetentionPolicyCode = "iys_netgsm_event"
	// Settle is how long a stored change waits before the feed lists it.
	Settle = time.Minute
	// MinWebhookToken is the shortest webhook secret.
	MinWebhookToken = 32
	// RefInHeader and RefInItem place Koro's reference (refid): Netgsm's
	// table says the header, its PHP package sends it in the item.
	RefInHeader = "header"
	RefInItem   = "item"

	dateLayout    = "2006-01-02 15:04:05"
	maxBody       = 1 << 20
	windowMargin  = 2 * time.Hour
	pageSize      = 1000
	purgeBatch    = 1000
	resultPoll    = 250 * time.Millisecond
	maxResultPoll = time.Second
)

// İYS recipient types.
const (
	RecipientIndividual = "BIREYSEL"
	RecipientMerchant   = "TACIR"
)

var (
	reCode     = regexp.MustCompile(`^[0-9]{1,12}$`)
	reHCode    = regexp.MustCompile(`^H[0-9]{1,5}$`)
	reID       = regexp.MustCompile(`^[0-9A-Za-z-]{1,128}$`)
	reToken    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	reRef      = regexp.MustCompile(`^[\x21-\x7e]{1,64}$`)
	reNumCode  = regexp.MustCompile(`^-?[0-9]{1,6}$`)
	transientH = map[string]bool{"H103": true, "H470": true, "H412": true, "H015": true, "H124": true, "H084": true, "H086": true}
)

var (
	// errAwaitingResult: Netgsm took the upload, İYS's answer has not
	// arrived by webhook yet; the caller retries.
	errAwaitingResult = errors.New("iys netgsm: Netgsm accepted the upload; İYS's answer has not arrived by webhook yet")
	// errNoAddress: the user has no address for the type (query asked
	// nothing).
	errNoAddress = errors.New("iys netgsm: no address")
)

// Recipients resolves users and İYS recipient addresses in both
// directions (identity, bound in cmd/koro/wire_consent.go); the address is
// used for the request or the lookup only. iys.ErrRecipientGone when there
// is none.
type Recipients interface {
	Recipient(ctx context.Context, user uuid.UUID, t iys.Type) (string, error)
	User(ctx context.Context, t iys.Type, address string) (uuid.UUID, error)
}

// Config configures the adapter.
type Config struct {
	BaseURL   string // DefaultBaseURL when empty (a path prefix is kept)
	Username  string // the Netgsm subscriber number (usercode)
	Password  string // the password of the "API user" sub-user
	IYSCode   string // the service provider's İYS number (webhook check)
	BrandCode string // the brand's İYS number
	// RecipientType is BIREYSEL (default) or TACIR.
	RecipientType string
	// RefIn places Koro's reference: RefInHeader (default) or RefInItem.
	RefIn string
	// WebhookToken is the secret of the webhook URL (≥ MinWebhookToken
	// characters of [A-Za-z0-9_-]).
	WebhookToken string
	// WebhookIPs, when set, are the only source addresses a delivery may
	// come from.
	WebhookIPs []netip.Prefix
	Recipients Recipients
	Store      Store
	Clock      clock.Clock
	// Log receives one debug line per call (operation, HTTP status, code)
	// and the receipt summaries; nil discards.
	Log     *slog.Logger
	Timeout time.Duration // per request; default 10s
	// RequestsPerMinute bounds every call (default 8, at most 10).
	RequestsPerMinute int
	// StatusQueriesPerHour bounds the single queries (default 240, at most 600).
	StatusQueriesPerHour int
	// ResultWait is how long Submit waits for İYS's answer by webhook
	// (default 15s; negative: no wait, one lookup).
	ResultWait time.Duration
	// ResendAfter is when an upload without an answer is sent again
	// (default 1h).
	ResendAfter time.Duration
	// RecheckSpacing separates the queries of the backup check (default:
	// two minutes / RequestsPerMinute, half of the per-minute budget).
	RecheckSpacing time.Duration
	// HTTPClient replaces the client built from Timeout (tests).
	HTTPClient *http.Client
}

// Client is the adapter: Submit, the change feed (Feed, Changes), single
// states (Statuses), the webhook (Receive) and the backup check (Recheck).
type Client struct {
	c      Config
	base   string
	feed   string
	token  [sha256.Size]byte
	client *http.Client

	mu      sync.Mutex
	calls   []time.Time // the last minute's calls
	queries []time.Time // the last hour's single queries
	sent    map[string]time.Time
}

// New validates c.
func New(c Config) (*Client, error) {
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = DefaultBaseURL
	}
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("iys netgsm: CONSENT_IYS_NETGSM_BASE_URL must be an http(s) URL without credentials or query")
	}
	switch {
	case c.Username == "" || c.Password == "":
		return nil, errors.New("iys netgsm: CONSENT_IYS_NETGSM_USERNAME and CONSENT_IYS_NETGSM_PASSWORD are required")
	case !reCode.MatchString(c.IYSCode) || !reCode.MatchString(c.BrandCode):
		return nil, errors.New("iys netgsm: CONSENT_IYS_NETGSM_IYS_CODE and CONSENT_IYS_NETGSM_BRAND_CODE must be İYS numbers")
	case len(c.WebhookToken) < MinWebhookToken || !reToken.MatchString(c.WebhookToken):
		return nil, fmt.Errorf("iys netgsm: CONSENT_IYS_NETGSM_WEBHOOK_TOKEN must be at least %d characters of [A-Za-z0-9_-]", MinWebhookToken)
	case c.Recipients == nil || c.Store == nil || c.Clock == nil:
		return nil, errors.New("iys netgsm: the adapter needs Recipients, Store and Clock")
	case c.RequestsPerMinute < 0 || c.RequestsPerMinute > 10 || c.StatusQueriesPerHour < 0 || c.StatusQueriesPerHour > 600:
		return nil, errors.New("iys netgsm: the budgets must stay within Netgsm's limit of 10 requests per minute")
	case c.ResendAfter < 0:
		return nil, errors.New("iys netgsm: ResendAfter must not be negative")
	}
	switch c.RecipientType {
	case "":
		c.RecipientType = RecipientIndividual
	case RecipientIndividual, RecipientMerchant:
	default:
		return nil, errors.New("iys netgsm: CONSENT_IYS_RECIPIENT_TYPE must be BIREYSEL or TACIR")
	}
	switch c.RefIn {
	case "":
		c.RefIn = RefInHeader
	case RefInHeader, RefInItem:
	default:
		return nil, errors.New("iys netgsm: CONSENT_IYS_NETGSM_REFID_IN must be header or item")
	}
	if c.RequestsPerMinute == 0 {
		c.RequestsPerMinute = DefaultRequestsPerMinute
	}
	if c.StatusQueriesPerHour == 0 {
		c.StatusQueriesPerHour = DefaultStatusQueriesPerHour
	}
	if c.ResultWait == 0 {
		c.ResultWait = DefaultResultWait
	}
	if c.ResendAfter == 0 {
		c.ResendAfter = DefaultResendAfter
	}
	if c.RecheckSpacing <= 0 {
		c.RecheckSpacing = 2 * time.Minute / time.Duration(c.RequestsPerMinute)
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
	return &Client{c: c, base: base, feed: "netgsm:" + c.IYSCode + "/" + c.BrandCode, token: sha256.Sum256([]byte(c.WebhookToken)),
		client: client, sent: map[string]time.Time{}}, nil
}

// Name names the adapter in logs (and the webhook path, consent's
// receiveIYSWebhook).
func (*Client) Name() string { return "netgsm" }

// Feed describes the change feed: one per İYS brand; the items are kept
// Retention (less a margin for late runs).
func (c *Client) Feed() iys.Feed { return iys.Feed{Name: c.feed, Window: Retention - windowMargin} }

// ---------------------------------------------------------------- Submit

// Submit registers one consent change and returns İYS's answer.
func (c *Client) Submit(ctx context.Context, con iys.Consent) (iys.Receipt, error) {
	switch {
	case !iys.ValidType(con.Type):
		return iys.Receipt{}, fmt.Errorf("%w: unknown İYS type", iys.ErrRejected)
	case con.Status != iys.Approved && con.Status != iys.Refused:
		return iys.Receipt{}, fmt.Errorf("%w: unknown İYS status", iys.ErrRejected)
	case iys.PlainSource(con.Source) == "" || con.ConsentDate.IsZero():
		return iys.Receipt{}, fmt.Errorf("%w: consent without source or date", iys.ErrRejected)
	case !reRef.MatchString(con.Ref):
		return iys.Receipt{}, fmt.Errorf("%w: consent without a reference", iys.ErrRejected)
	}
	recipient, err := c.c.Recipients.Recipient(ctx, con.UserID, con.Type)
	switch {
	case errors.Is(err, iys.ErrRecipientGone) || (err == nil && recipient == ""):
		return iys.Receipt{}, fmt.Errorf("%w: %w", iys.ErrRejected, iys.ErrRecipientGone)
	case err != nil:
		return iys.Receipt{}, fmt.Errorf("iys netgsm: recipient: %w", err)
	}
	at := con.ConsentDate.UTC().Truncate(time.Second)
	q := ResultQuery{Ref: con.Ref, UserID: con.UserID, Type: con.Type, Status: con.Status, ConsentAt: at}
	r, found, err := c.c.Store.Result(ctx, c.feed, q)
	if err != nil {
		return iys.Receipt{}, err
	}
	if found {
		if rc, done, err := c.receipt(ctx, con, r); done {
			return rc, err
		}
	}
	if !c.due(con.Ref, r, found) {
		return iys.Receipt{}, errAwaitingResult // sent recently: the answer is on its way (no wait here)
	}
	uid, err := c.add(ctx, con, recipient, at)
	if err != nil {
		return iys.Receipt{}, err
	}
	c.markSent(con.Ref)
	c.c.Log.LogAttrs(ctx, slog.LevelDebug, "iys netgsm upload accepted", slog.String("ref", con.Ref), slog.String("submit_id", uid))
	r, found, err = c.await(ctx, q, found, r.Seq)
	if err != nil {
		return iys.Receipt{}, err
	}
	if found {
		if rc, done, err := c.receipt(ctx, con, r); done {
			return rc, err
		}
	}
	return iys.Receipt{}, errAwaitingResult
}

// receipt maps İYS's answer r to Submit's result; done is false for a
// transient failure (send again).
func (c *Client) receipt(ctx context.Context, con iys.Consent, r Event) (iys.Receipt, bool, error) {
	if r.Result == ResultSuccess {
		tx := r.TransactionID
		if tx == "" {
			tx = "netgsm:" + r.SubmitID
		}
		return iys.Receipt{TransactionID: tx}, true, nil
	}
	switch {
	case con.Status == iys.Refused && (r.ErrCode == "H174" || r.ErrCode == "H175"):
		c.c.Log.LogAttrs(ctx, slog.LevelDebug, "iys refusal already held", slog.String("code", r.ErrCode), slog.String("ref", con.Ref))
		return iys.Receipt{AlreadyHeld: true}, true, nil
	case r.ErrCode == "H467":
		return iys.Receipt{}, true, fmt.Errorf("iys netgsm: İYS answered H467: %w", errors.Join(iys.ErrRejected, iys.ErrTooLate))
	case transientH[r.ErrCode]:
		return iys.Receipt{}, false, nil
	}
	code := r.ErrCode
	if code == "" {
		code = "without a code"
	}
	return iys.Receipt{}, true, fmt.Errorf("%w: İYS refused the upload (%s)", iys.ErrRejected, code)
}

// due reports whether the upload of ref is sent (again): never sent by
// this process, sent longer than ResendAfter ago without an answer, or
// answered with a transient failure after it was sent.
func (c *Client) due(ref string, last Event, answered bool) bool {
	now := c.c.Clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.sent[ref]
	switch {
	case !ok:
		return true
	case answered && !last.ReceivedAt.Before(at):
		return true // the answer to this sending was a transient failure
	}
	return now.Sub(at) >= c.c.ResendAfter
}

func (c *Client) markSent(ref string) {
	now := c.c.Clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for r, at := range c.sent {
		if now.Sub(at) > 2*c.c.ResendAfter {
			delete(c.sent, r)
		}
	}
	c.sent[ref] = now
}

// await polls the store for an answer newer than seq (when known was
// true) for up to ResultWait.
func (c *Client) await(ctx context.Context, q ResultQuery, known bool, seq int64) (Event, bool, error) {
	var deadline <-chan time.Time
	if c.c.ResultWait > 0 {
		t := time.NewTimer(c.c.ResultWait)
		defer t.Stop()
		deadline = t.C
	}
	poll := resultPoll
	for {
		r, found, err := c.c.Store.Result(ctx, c.feed, q)
		if err != nil || (found && (!known || r.Seq > seq)) {
			return r, found, err
		}
		if deadline == nil {
			return Event{}, false, nil
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return Event{}, false, ctx.Err()
		case <-deadline:
			t.Stop()
			return Event{}, false, nil
		case <-t.C:
		}
		poll = min(2*poll, maxResultPoll)
	}
}

type header struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	BrandCode string `json:"brandCode"`
	RefID     string `json:"refid,omitempty"`
}

type addItem struct {
	Type          string `json:"type"`
	Source        string `json:"source"`
	Recipient     string `json:"recipient"`
	Status        string `json:"status"`
	ConsentDate   string `json:"consentDate"`
	RecipientType string `json:"recipientType"`
	RefID         string `json:"refid,omitempty"`
}

type request[T any] struct {
	Header header `json:"header"`
	Body   struct {
		Data []T `json:"data"`
	} `json:"body"`
}

// add uploads one consent and returns Netgsm's request ID.
func (c *Client) add(ctx context.Context, con iys.Consent, recipient string, at time.Time) (string, error) {
	item := addItem{Type: string(con.Type), Source: con.Source, Recipient: recipient, Status: string(con.Status),
		ConsentDate: at.In(clock.Istanbul()).Format(dateLayout), RecipientType: c.c.RecipientType}
	req := request[addItem]{Header: c.header()}
	if c.c.RefIn == RefInItem {
		item.RefID = con.Ref
	} else {
		req.Header.RefID = con.Ref
	}
	req.Body.Data = []addItem{item}
	var out answer
	if err := c.call(ctx, "add", "/iys/add", req, &out); err != nil {
		return "", err
	}
	// The only item was refused for its form (erroritem lists it, with the
	// address: never read); the code is 0 or 70.
	if e := bytes.TrimSpace(out.ErrorItem); len(e) > 0 && !slices.Contains([]string{"null", "{}", "[]"}, string(e)) {
		return "", fmt.Errorf("%w: iys netgsm: add: code %s (Netgsm refused the address form: erroritem)", iys.ErrRejected,
			safeCode(out.code()))
	}
	if err := out.check("add"); err != nil {
		return "", err
	}
	uid := string(out.UID)
	if !reID.MatchString(uid) {
		return "", errors.New("iys netgsm: add: malformed answer (uid)")
	}
	return uid, nil
}

func (c *Client) header() header {
	return header{Username: c.c.Username, Password: c.c.Password, BrandCode: c.c.BrandCode}
}

// ---------------------------------------------------------------- Statuses

type searchItem struct {
	Type          string `json:"type"`
	Recipient     string `json:"recipient"`
	RecipientType string `json:"recipientType"`
}

type queryAnswer struct {
	Status        flex `json:"status"`
	Source        flex `json:"source"`
	ConsentDate   flex `json:"consentDate"`
	CreationDate  flex `json:"creationDate"`
	TransactionID flex `json:"transactionId"`
	// recipient, recipientType and type are not needed.
}

// Statuses returns İYS's current state of the users' addresses for t
// (one /iys/search per address, within the budgets).
func (c *Client) Statuses(ctx context.Context, t iys.Type, userIDs []uuid.UUID) (map[uuid.UUID]iys.State, error) {
	switch {
	case !iys.ValidType(t):
		return nil, fmt.Errorf("%w: unknown İYS type", iys.ErrRejected)
	case len(userIDs) > iys.MaxStatusBatch:
		return nil, fmt.Errorf("iys netgsm: %d users in one status read (at most %d)", len(userIDs), iys.MaxStatusBatch)
	}
	out := make(map[uuid.UUID]iys.State, len(userIDs))
	seen := make(map[uuid.UUID]bool, len(userIDs))
	for _, id := range userIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		st, _, found, err := c.query(ctx, t, id)
		switch {
		case errors.Is(err, errNoAddress):
			continue
		case err != nil:
			return nil, err
		}
		if found {
			out[id] = st
		}
	}
	return out, nil
}

// query is the single query of one user's address: İYS's state and the
// record's transaction ID ("" when absent). errNoAddress when the user has
// no address for t (nothing asked).
func (c *Client) query(ctx context.Context, t iys.Type, user uuid.UUID) (iys.State, string, bool, error) {
	a, err := c.c.Recipients.Recipient(ctx, user, t)
	switch {
	case errors.Is(err, iys.ErrRecipientGone) || (err == nil && a == ""):
		return iys.State{}, "", false, errNoAddress
	case err != nil:
		return iys.State{}, "", false, fmt.Errorf("iys netgsm: recipient: %w", err)
	}
	if err := c.takeQuery(); err != nil {
		return iys.State{}, "", false, err
	}
	req := request[searchItem]{Header: c.header()}
	req.Body.Data = []searchItem{{Type: string(t), Recipient: a, RecipientType: c.c.RecipientType}}
	var out answer
	if err := c.call(ctx, "search", "/iys/search", req, &out); err != nil {
		return iys.State{}, "", false, err
	}
	if out.code() == "50" {
		return iys.State{}, "", false, nil // no record
	}
	if err := out.check("search"); err != nil {
		return iys.State{}, "", false, err
	}
	var q queryAnswer
	if raw := bytes.TrimSpace(out.Query); len(raw) > 0 && raw[0] == '[' { // tolerate a list of one
		var list []queryAnswer
		if json.Unmarshal(raw, &list) != nil {
			return iys.State{}, "", false, errors.New("iys netgsm: search: malformed answer (query)")
		}
		if len(list) > 0 {
			q = list[0]
		}
	} else if len(raw) > 0 && json.Unmarshal(raw, &q) != nil {
		return iys.State{}, "", false, errors.New("iys netgsm: search: malformed answer (query)")
	}
	switch iys.Status(q.Status) {
	case "":
		return iys.State{}, "", false, nil
	case iys.Approved, iys.Refused:
	default:
		return iys.State{}, "", false, errors.New("iys netgsm: search: malformed answer (status)")
	}
	st := iys.State{Status: iys.Status(q.Status), Source: iys.PlainSource(string(q.Source))}
	if at, err := parseDate(string(q.ConsentDate)); err == nil {
		st.At = at
	} else if at, err := parseDate(string(q.CreationDate)); err == nil {
		st.At = at
	}
	tx := strings.TrimSpace(string(q.TransactionID))
	if !reID.MatchString(tx) {
		tx = ""
	}
	return st, tx, true, nil
}

// ---------------------------------------------------------------- HTTP

// answer is Netgsm's answer: code and error come as strings or numbers.
type answer struct {
	Code        flex            `json:"code"`
	Error       flex            `json:"error"`
	UID         flex            `json:"uid"`
	ErrorItem   json.RawMessage `json:"erroritem"`
	Query       json.RawMessage `json:"query"`
	Description flex            `json:"description"`
}

func (a answer) code() string { return strings.TrimSpace(string(a.Code)) }

// check classifies a non-zero code.
func (a answer) check(op string) error {
	code := a.code()
	switch code {
	case "0", "00":
		return nil
	case "":
		return fmt.Errorf("iys netgsm: %s: malformed answer (no code)", op)
	case "30":
		return &apiError{op: op, code: code, text: "credentials, API access or the IP restriction refused", class: []error{iys.ErrRejected}}
	case "40":
		return &apiError{op: op, code: code, text: "the İYS module of the account is not active", class: []error{iys.ErrRejected}}
	case "60":
		return &apiError{op: op, code: code, text: "the brand code is not the account's", class: []error{iys.ErrRejected}}
	case "70":
		return &apiError{op: op, code: code, text: "parameter error", class: []error{iys.ErrRejected}}
	case "8010":
		return &apiError{op: op, code: code, text: "too many addresses in one request", class: []error{iys.ErrRejected}}
	case "100", "101":
		return &apiError{op: op, code: code, text: "Netgsm system error or rate limit (10 requests per minute)", class: []error{iys.ErrThrottled}}
	}
	return &apiError{op: op, code: safeCode(code), text: "unknown Netgsm code"}
}

// safeCode is code when it is a plain number ("?" otherwise: an answer's
// text is never repeated).
func safeCode(code string) string {
	if !reNumCode.MatchString(code) {
		return "?"
	}
	return code
}

// apiError is a refused call: Netgsm's code (or the HTTP status) and a
// fixed text; it unwraps to the port error of its class.
type apiError struct {
	op     string
	status int
	code   string
	text   string
	class  []error
}

func (e *apiError) Error() string {
	s := "iys netgsm: " + e.op + ": "
	if e.status != 0 {
		s += fmt.Sprintf("HTTP %d", e.status)
	} else {
		s += "code " + e.code
	}
	if e.text != "" {
		s += " (" + e.text + ")"
	}
	return s
}

func (e *apiError) Unwrap() []error { return e.class }

// call posts body to path and decodes the answer into out (it must be a
// 2xx JSON answer; a bare number is taken as the code).
func (c *Client) call(ctx context.Context, op, path string, body any, out *answer) error {
	if err := c.takeCall(); err != nil {
		return err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("iys netgsm: %s: encode: %w", op, err)
	}
	defer clear(raw) // holds the password and the address
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("iys netgsm: %s: cannot build the request", op)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("iys netgsm: %s: %s", op, scrub(err.Error()))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	defer clear(data) // may hold the address
	c.c.Log.LogAttrs(ctx, slog.LevelDebug, "iys netgsm call", slog.String("op", op), slog.Int("status", resp.StatusCode))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &apiError{op: op, status: resp.StatusCode, class: []error{iys.ErrThrottled}}
	case resp.StatusCode >= 500:
		return &apiError{op: op, status: resp.StatusCode}
	case resp.StatusCode/100 != 2:
		return &apiError{op: op, status: resp.StatusCode, class: []error{iys.ErrRejected}}
	}
	trimmed := bytes.TrimSpace(data)
	if reNumCode.Match(trimmed) {
		*out = answer{Code: flex(trimmed)}
		return nil
	}
	if json.Unmarshal(trimmed, out) != nil {
		return fmt.Errorf("iys netgsm: %s: malformed answer", op)
	}
	return nil
}

// takeCall spends one call of the per-minute budget.
func (c *Client) takeCall() error {
	now := c.c.Clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = within(c.calls, now, time.Minute)
	if len(c.calls) >= c.c.RequestsPerMinute {
		return fmt.Errorf("%w: Netgsm call budget of %d per minute spent", iys.ErrThrottled, c.c.RequestsPerMinute)
	}
	c.calls = append(c.calls, now)
	return nil
}

// takeQuery spends one single query of the hourly budget.
func (c *Client) takeQuery() error {
	now := c.c.Clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = within(c.queries, now, time.Hour)
	if len(c.queries) >= c.c.StatusQueriesPerHour {
		return fmt.Errorf("%w: Netgsm single query budget of %d per hour spent", iys.ErrThrottled, c.c.StatusQueriesPerHour)
	}
	c.queries = append(c.queries, now)
	return nil
}

func within(ts []time.Time, now time.Time, d time.Duration) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < d {
			out = append(out, t)
		}
	}
	return out
}

// flex is a JSON scalar read as text: Netgsm sends codes and IDs as
// strings or numbers, flags as strings or booleans.
type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || bytes.Equal(b, []byte("null")):
		*f = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flex(s)
	case b[0] == '{' || b[0] == '[':
		*f = "" // not a scalar: ignored
	default:
		*f = flex(b)
	}
	return nil
}

func parseDate(s string) (time.Time, error) {
	t, err := time.ParseInLocation(dateLayout, strings.TrimSpace(s), clock.Istanbul())
	return t.UTC(), err
}

// scrub removes addresses and control characters from a transport error.
func scrub(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, obs.Scrub(s))
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300])
	}
	return s
}
