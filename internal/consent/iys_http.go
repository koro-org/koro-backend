package consent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/obs"
)

// İYS API (İleti Yönetim Sistemi, hizmet sağlayıcı entegrasyonu; H-14,
// T-159). The adapter adds one consent record per submission:
//
//	POST {base}/oauth2/token                                {username, password, grant_type=password}
//	POST {base}/sps/{iysCode}/brands/{brandCode}/consents   {consentDate, source, recipient, recipientType, status, type}
//
// Mapping: action granted → ONAY, withdrawn → RET; channel email → EPOSTA,
// sms → MESAJ, call → ARAMA; consents.channel → source (web → HS_WEB,
// mobile → HS_MOBIL, email_link → HS_EPOSTA, admin/import → HS_WEB, T-184);
// consentDate is occurred_at in Europe/Istanbul ("2006-01-02 15:04:05").
//
// Errors: 400/409/422 (İYS refused the record) and a recipient that is gone
// wrap ErrIYSPermanent; 401 renews the access token once; 429, 5xx and
// transport errors are transient (the task and consent.iys_reconcile retry).
// The recipient address, the password and the tokens never appear in an
// error or a log line (İYS error texts are scrubbed with obs.Scrub).
//
// IYSHTTP does not read İYS's consent states (IYSStatusReader, BE-103):
// those İYS calls are not part of this adapter yet (00 T-354), so
// consent.iys_status_sync is not scheduled with it and
// CONSENT_IYS_SYNC_MAX_AGE refuses to start.
const (
	iysDateLayout    = "2006-01-02 15:04:05"
	iysTokenMargin   = time.Minute
	iysMaxBody       = 64 << 10
	iysMaxError      = 300
	iysDefaultExpiry = 2 * time.Hour
)

// İYS recipient types.
const (
	IYSRecipientIndividual = "BIREYSEL"
	IYSRecipientMerchant   = "TACIR"
)

var reIYSCode = regexp.MustCompile(`^[0-9]{1,12}$`)

// IYSHTTPConfig configures IYSHTTP.
type IYSHTTPConfig struct {
	BaseURL       string // https://api.iys.org.tr
	Username      string
	Password      string
	IYSCode       string // the service provider's İYS number
	BrandCode     string // the brand's İYS number
	RecipientType string // BIREYSEL (default) | TACIR
	Recipients    IYSRecipients
	Timeout       time.Duration
	Clock         clock.Clock
	// HTTPClient replaces the client built from Timeout (tests).
	HTTPClient *http.Client
}

// IYSHTTP is the real İYS client.
type IYSHTTP struct {
	c      IYSHTTPConfig
	base   string
	client *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

var _ IYSClient = (*IYSHTTP)(nil)

// NewIYSHTTP validates c.
func NewIYSHTTP(c IYSHTTPConfig) (*IYSHTTP, error) {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, errors.New("consent: CONSENT_IYS_BASE_URL must be an http(s) URL without credentials")
	}
	switch {
	case c.Username == "" || c.Password == "":
		return nil, errors.New("consent: CONSENT_IYS_USERNAME and CONSENT_IYS_PASSWORD are required")
	case !reIYSCode.MatchString(c.IYSCode) || !reIYSCode.MatchString(c.BrandCode):
		return nil, errors.New("consent: CONSENT_IYS_CODE and CONSENT_IYS_BRAND_CODE must be İYS numbers")
	case c.Recipients == nil || c.Clock == nil:
		return nil, errors.New("consent: the İYS client needs Recipients and Clock")
	}
	switch c.RecipientType {
	case "":
		c.RecipientType = IYSRecipientIndividual
	case IYSRecipientIndividual, IYSRecipientMerchant:
	default:
		return nil, errors.New("consent: CONSENT_IYS_RECIPIENT_TYPE must be BIREYSEL or TACIR")
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
	return &IYSHTTP{c: c, base: base, client: client}, nil
}

// Name implements IYSClient.
func (*IYSHTTP) Name() string { return "iys" }

// Real implements IYSClient.
func (*IYSHTTP) Real() bool { return true }

type iysConsent struct {
	ConsentDate   string `json:"consentDate"`
	Source        string `json:"source"`
	Recipient     string `json:"recipient"`
	RecipientType string `json:"recipientType"`
	Status        string `json:"status"`
	Type          string `json:"type"`
}

// Submit implements IYSClient.
func (h *IYSHTTP) Submit(ctx context.Context, r IYSRecord) (IYSReceipt, error) {
	typ, ok := map[IYSChannel]string{IYSEmail: "EPOSTA", IYSSMS: "MESAJ", IYSCall: "ARAMA"}[r.Channel]
	if !ok {
		return IYSReceipt{}, fmt.Errorf("%w: unknown İYS channel %q", ErrIYSPermanent, r.Channel)
	}
	status, ok := map[Action]string{Granted: "ONAY", Withdrawn: "RET"}[r.Action]
	if !ok {
		return IYSReceipt{}, fmt.Errorf("%w: unknown action %q", ErrIYSPermanent, r.Action)
	}
	recipient, err := h.c.Recipients.IYSRecipient(ctx, r.UserID, r.Channel)
	switch {
	case errors.Is(err, ErrIYSRecipientGone):
		return IYSReceipt{}, fmt.Errorf("%w: %w", ErrIYSPermanent, ErrIYSRecipientGone)
	case err != nil:
		return IYSReceipt{}, fmt.Errorf("consent: İYS recipient: %w", err)
	case recipient == "":
		return IYSReceipt{}, fmt.Errorf("%w: %w", ErrIYSPermanent, ErrIYSRecipientGone)
	}
	body, err := json.Marshal(iysConsent{
		ConsentDate: r.OccurredAt.In(clock.Istanbul()).Format(iysDateLayout), Source: iysSource(r.Source),
		Recipient: recipient, RecipientType: h.c.RecipientType, Status: status, Type: typ,
	})
	if err != nil {
		return IYSReceipt{}, fmt.Errorf("consent: İYS encode: %w", err)
	}
	path := "/sps/" + h.c.IYSCode + "/brands/" + h.c.BrandCode + "/consents"
	for attempt := 0; ; attempt++ {
		tok, err := h.accessToken(ctx, attempt > 0)
		if err != nil {
			return IYSReceipt{}, err
		}
		var out struct {
			TransactionID string `json:"transactionId"`
		}
		status, err := h.post(ctx, path, "Bearer "+tok, body, &out)
		if status == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		if err != nil {
			return IYSReceipt{}, err
		}
		return IYSReceipt{TransactionID: out.TransactionID}, nil
	}
}

// iysSource maps consents.channel to the İYS source (T-184: admin and
// import to be confirmed with İYS / counsel).
func iysSource(ch Channel) string {
	switch ch {
	case ChannelMobile:
		return "HS_MOBIL"
	case ChannelEmailLink:
		return "HS_EPOSTA"
	default: // web, admin (erasure on the user's request), import
		return "HS_WEB"
	}
}

// accessToken returns the cached token or signs in again.
func (h *IYSHTTP) accessToken(ctx context.Context, force bool) (string, error) {
	now := h.c.Clock.Now()
	h.mu.Lock()
	if !force && h.token != "" && now.Before(h.expiry.Add(-iysTokenMargin)) {
		tok := h.token
		h.mu.Unlock()
		return tok, nil
	}
	h.mu.Unlock()
	body, err := json.Marshal(map[string]string{"username": h.c.Username, "password": h.c.Password, "grant_type": "password"})
	if err != nil {
		return "", err
	}
	defer clear(body)
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if _, err := h.post(ctx, "/oauth2/token", "", body, &out); err != nil {
		if errors.Is(err, ErrIYSPermanent) { // a refused sign-in is a configuration problem: retry later
			return "", errors.New("consent: İYS sign-in refused: " + strings.TrimPrefix(err.Error(), ErrIYSPermanent.Error()+": "))
		}
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("consent: İYS sign-in: no access token in the answer")
	}
	exp := time.Duration(out.ExpiresIn) * time.Second
	if exp <= 0 {
		exp = iysDefaultExpiry
	}
	h.mu.Lock()
	h.token, h.expiry = out.AccessToken, now.Add(exp)
	h.mu.Unlock()
	return out.AccessToken, nil
}

// post sends one JSON request. Errors carry the status and İYS's scrubbed
// error codes, never the request.
func (h *IYSHTTP) post(ctx context.Context, path, auth string, body []byte, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("consent: İYS: cannot build the request")
	}
	req.Header.Set("Content-Type", "application/json")
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
		return 0, fmt.Errorf("consent: İYS %s: %s", path, iysText(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, iysMaxBody))
	if resp.StatusCode/100 == 2 {
		if out != nil && json.Unmarshal(raw, out) != nil {
			return resp.StatusCode, fmt.Errorf("consent: İYS %s: malformed answer", path)
		}
		return resp.StatusCode, nil
	}
	err = fmt.Errorf("consent: İYS %s: HTTP %d: %s", path, resp.StatusCode, iysErrors(raw))
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		return resp.StatusCode, fmt.Errorf("%w: %w", ErrIYSPermanent, err)
	default:
		return resp.StatusCode, err
	}
}

// iysErrors extracts the error codes and messages of an İYS error answer
// ({"errors":[{"code":"H174","message":"…"}]} or {"message":"…"}), scrubbed.
func iysErrors(raw []byte) string {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &e)
	parts := make([]string, 0, len(e.Errors)+1)
	for _, x := range e.Errors {
		parts = append(parts, strings.TrimSpace(x.Code+" "+x.Message))
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if len(parts) == 0 {
		return "no error text"
	}
	return iysText(strings.Join(parts, "; "))
}

func iysText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, obs.Scrub(s))
	return cut(s, iysMaxError)
}
