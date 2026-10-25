package netgsm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"uuid"

	"koro/internal/platform/iys"
)

// MaxItems is the largest number of items one webhook delivery may carry.
const MaxItems = 1000

// item is one webhook item. encoding/json matches the names
// case-insensitively (Netgsm's README writes consentdate and recipienttype,
// its documentation consentDate and recipientType). The recipient is read
// for the user lookup only; errmsg (it repeats the address), the
// retailer fields and any other field are never decoded.
type item struct {
	IYSCode       flex `json:"iyscode"`
	BrandCode     flex `json:"brandcode"`
	SubmitID      flex `json:"submitid"`
	ResultStatus  flex `json:"resultstatus"`
	ErrCode       flex `json:"errcode"`
	CreationDate  flex `json:"creationdate"`
	UserRefCode   flex `json:"user_refcode"`
	TransactionID flex `json:"transactionid"`
	Type          flex `json:"type"`
	Source        flex `json:"source"`
	Recipient     flex `json:"recipient"`
	Status        flex `json:"status"`
	ConsentDate   flex `json:"consentDate"`
	RecipientType flex `json:"recipientType"`
}

// receiptTally counts one delivery (logged; never personal data).
type receiptTally struct {
	items, foreign, malformed, ignored, strangers int
}

// Receive stores one webhook delivery (consent's receiveIYSWebhook). It
// returns how many items were new; a redelivery stores nothing and
// succeeds. iys.ErrUnauthenticated when the token or the source address
// is wrong, iys.ErrRejected when the body is not Netgsm's item list;
// any other error is transient (the delivery should be retried).
func (c *Client) Receive(ctx context.Context, d iys.Delivery) (int, error) {
	if !c.authentic(d) {
		return 0, iys.ErrUnauthenticated
	}
	items, err := decodeItems(d.Body)
	if err != nil {
		return 0, fmt.Errorf("%w: iys netgsm: webhook: %s", iys.ErrRejected, err.Error())
	}
	now := c.c.Clock.Now().UTC()
	t := receiptTally{items: len(items)}
	events := make([]Event, 0, len(items))
	for i := range items {
		e, ok, err := c.event(ctx, &items[i], now, &t)
		if err != nil {
			return 0, err
		}
		if ok {
			events = append(events, e)
		}
	}
	added, err := c.c.Store.Append(ctx, c.feed, events)
	if err != nil {
		return 0, err
	}
	level := slog.LevelInfo
	if t.foreign > 0 || t.malformed > 0 {
		level = slog.LevelWarn
	}
	c.c.Log.LogAttrs(ctx, level, "iys netgsm webhook received", slog.Int("items", t.items), slog.Int("stored", len(events)),
		slog.Int("new", added), slog.Int("strangers", t.strangers), slog.Int("foreign", t.foreign),
		slog.Int("malformed", t.malformed), slog.Int("ignored", t.ignored))
	return added, nil
}

// authentic checks the secret of the URL (constant time over digests, so
// the length leaks nothing) and the source address.
func (c *Client) authentic(d iys.Delivery) bool {
	got := sha256.Sum256([]byte(d.Token))
	ok := subtle.ConstantTimeCompare(got[:], c.token[:]) == 1
	if !ok || len(c.c.WebhookIPs) == 0 {
		return ok
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(d.ClientIP))
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, p := range c.c.WebhookIPs {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// decodeItems reads the delivery: a list of items (Netgsm's
// documentation) or a single item (its PHP package's example).
func decodeItems(body []byte) ([]item, error) {
	raw := bytes.TrimSpace(body)
	switch {
	case len(raw) == 0:
		return nil, errors.New("empty body")
	case raw[0] == '{':
		var it item
		if err := json.Unmarshal(raw, &it); err != nil {
			return nil, errors.New("malformed JSON item")
		}
		return []item{it}, nil
	case raw[0] == '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, errors.New("malformed JSON list")
		}
		if len(items) > MaxItems {
			return nil, fmt.Errorf("%d items (at most %d)", len(items), MaxItems)
		}
		out := make([]item, len(items))
		for i, r := range items {
			if json.Unmarshal(r, &out[i]) != nil {
				return nil, fmt.Errorf("malformed item %d", i)
			}
		}
		return out, nil
	}
	return nil, errors.New("not a JSON item list")
}

// event turns an item into what is stored; ok is false for an item that
// is not kept (counted in t). The address is resolved and forgotten.
func (c *Client) event(ctx context.Context, it *item, now time.Time, t *receiptTally) (Event, bool, error) {
	defer func() { it.Recipient = "" }()
	if strings.TrimSpace(string(it.IYSCode)) != c.c.IYSCode || strings.TrimSpace(string(it.BrandCode)) != c.c.BrandCode {
		t.foreign++ // another brand or service provider: never ours
		return Event{}, false, nil
	}
	e := Event{
		Type: iys.Type(strings.TrimSpace(string(it.Type))), Status: iys.Status(strings.TrimSpace(string(it.Status))),
		Source: iys.PlainSource(strings.TrimSpace(string(it.Source))), ReceivedAt: now,
	}
	if !iys.ValidType(e.Type) || (e.Status != iys.Approved && e.Status != iys.Refused) {
		t.malformed++
		return Event{}, false, nil
	}
	if at, err := parseDate(string(it.ConsentDate)); err == nil {
		e.ConsentAt = at
	}
	if at, err := parseDate(string(it.CreationDate)); err == nil {
		e.RecordedAt = at
	}
	result := strings.ToLower(strings.TrimSpace(string(it.ResultStatus)))
	ownType := def(strings.TrimSpace(string(it.RecipientType)), RecipientIndividual) == c.c.RecipientType
	if strings.HasPrefix(e.Source, "IYS_") {
		e.Kind = KindChange
		switch {
		case result == ResultFailure:
			t.ignored++ // İYS did not take it: nothing changed
			return Event{}, false, nil
		case e.ConsentAt.IsZero() && e.RecordedAt.IsZero():
			t.malformed++
			return Event{}, false, nil
		}
	} else {
		e.Kind = KindResult
		if result != ResultSuccess && result != ResultFailure {
			t.malformed++
			return Event{}, false, nil
		}
		e.Result = result
		if code := strings.TrimSpace(string(it.ErrCode)); reHCode.MatchString(code) {
			e.ErrCode = code
		}
		if ref := strings.TrimSpace(string(it.UserRefCode)); reRef.MatchString(ref) {
			e.Ref = ref
		}
		if !ownType {
			t.ignored++
			return Event{}, false, nil
		}
	}
	if id := strings.TrimSpace(string(it.SubmitID)); reID.MatchString(id) {
		e.SubmitID = id
	}
	if tx := strings.TrimSpace(string(it.TransactionID)); reID.MatchString(tx) {
		e.TransactionID = tx
	}
	if address := strings.TrimSpace(string(it.Recipient)); address != "" && ownType {
		user, err := c.c.Recipients.User(ctx, e.Type, address)
		switch {
		case errors.Is(err, iys.ErrRecipientGone):
		case err != nil:
			return Event{}, false, fmt.Errorf("iys netgsm: webhook: recipient: %w", err)
		default:
			e.UserID = user
		}
	}
	switch {
	case e.UserID == uuid.Nil() && e.Kind == KindResult && e.Ref == "":
		t.ignored++ // an answer to an upload Koro cannot match
		return Event{}, false, nil
	case e.UserID == uuid.Nil():
		t.strangers++ // kept as an anonymous marker (Skipped)
	}
	e.Key = eventKey(c.feed, e)
	return e, true, nil
}

// eventKey is the de-duplication key of an event: İYS's transaction ID
// identifies a change (the webhook and the backup check see the same
// one); otherwise, and for the answers to uploads, every field counts.
func eventKey(feed string, e Event) string {
	if e.Kind == KindChange && e.TransactionID != "" {
		return digest("change", feed, e.TransactionID)
	}
	return digest(string(e.Kind), feed, e.Ref, e.SubmitID, e.TransactionID, e.UserID.String(), string(e.Type),
		string(e.Status), e.Source, stamp(e.ConsentAt), stamp(e.RecordedAt), e.Result, e.ErrCode)
}

// Changes returns the next page of stored changes after cursor (the
// store's sequence; "" from the start of the retention).
func (c *Client) Changes(ctx context.Context, cursor string) (iys.ChangePage, error) {
	var after int64
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 0 || !iys.ValidCursor(cursor) {
			return iys.ChangePage{}, fmt.Errorf("%w: malformed cursor", iys.ErrRejected)
		}
		after = n
	}
	now := c.c.Clock.Now().UTC()
	until := now.Add(-Settle)
	evs, err := c.c.Store.Changes(ctx, c.feed, after, until, pageSize)
	if err != nil {
		return iys.ChangePage{}, err
	}
	page := iys.ChangePage{Cursor: cursor, Through: until}
	for _, e := range evs {
		page.Cursor = strconv.FormatInt(e.Seq, 10)
		if e.UserID == uuid.Nil() {
			page.Skipped++
			continue
		}
		st := iys.State{Status: e.Status, At: e.ConsentAt, Source: e.Source}
		if st.At.IsZero() {
			st.At = e.RecordedAt
		}
		recorded := e.RecordedAt
		if recorded.IsZero() {
			recorded = e.ReceivedAt
		}
		page.Changes = append(page.Changes, iys.Change{UserID: e.UserID, Type: e.Type, State: st, RecordedAt: recorded})
	}
	if len(evs) == pageSize {
		page.More, page.Through = true, evs[len(evs)-1].ReceivedAt
	}
	return page, nil
}

// digest is the de-duplication key of an item.
func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0x1f})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return strconv.FormatInt(t.Unix(), 10)
}

func def(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
