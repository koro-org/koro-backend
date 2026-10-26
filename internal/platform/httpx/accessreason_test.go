package httpx_test

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
)

type reasonInput struct {
	httpx.AccessReasonHeader
}

type reasonOutput struct {
	Body struct {
		Reason string `json:"reason"`
	}
}

// reasonApp serves GET /api/v1/reason, which answers the decoded header.
func reasonApp(t *testing.T) *testApp {
	t.Helper()
	a := newTestApp(t, nil)
	op.Register(a.V1, "audit", huma.Operation{OperationID: "echoReason", Method: http.MethodGet, Path: "/reason"},
		op.Meta{Action: "audit.event.list", Surface: op.SurfacePublic},
		func(_ context.Context, in *reasonInput) (*reasonOutput, error) {
			out := &reasonOutput{}
			out.Body.Reason = in.AccessReason.String()
			return out, nil
		})
	return a
}

// encode is what koro-web sends, encodeURIComponent behind the UTF-8
// ext-value prefix (url.PathEscape leaves the same unreserved characters).
func encode(s string) string { return "UTF-8''" + url.PathEscape(s) }

// TestAccessReason_DecodesUTF8 (BE-128): a Turkish justification arrives
// as an RFC 8187 ext-value and reaches the operation as typed; a plain
// ASCII value passes as before; either is normalized (NFC, one space).
func TestAccessReason_DecodesUTF8(t *testing.T) {
	t.Parallel()
	a := reasonApp(t)
	turkish := "Destek talebi 4711: öğrenci şifre sıfırlama e-postası almıyor (İzmir, ÇĞÖŞÜ)"
	for name, c := range map[string]struct{ header, want string }{
		"plain ASCII":        {"Ticket 4711: user reports missing e-mails", "Ticket 4711: user reports missing e-mails"},
		"encoded Turkish":    {encode(turkish), turkish},
		"lower-case charset": {"utf-8''" + url.PathEscape(turkish), turkish},
		"language tag":       {"UTF-8'tr'" + url.PathEscape(turkish), turkish},
		"plus stays plus":    {"UTF-8''a+b%20%C3%A7", "a+b ç"},
		"decomposed to NFC":  {encode("şifre sorunu destek kaydı"), "şifre sorunu destek kaydı"},
		"spaces collapse":    {"Ticket  4711:   resend", "Ticket 4711: resend"},
		"absent":             {"", ""},
	} {
		hdr := map[string]string{}
		if c.header != "" {
			hdr[httpx.HeaderAccessReason] = c.header
		}
		rec := a.do(t, http.MethodGet, "/api/v1/reason", nil, hdr)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reason":"`+c.want+`"`) {
			t.Errorf("%s: %d %s, want reason %q", name, rec.Code, rec.Body.String(), c.want)
		}
	}
}

// TestAccessReason_MalformedIs422: a broken encoding, bytes that are not
// UTF-8 and control or bidi characters (display spoofing in the notice the
// user reads) are refused at the header.
func TestAccessReason_MalformedIs422(t *testing.T) {
	t.Parallel()
	a := reasonApp(t)
	for name, header := range map[string]string{
		"bad percent":      "UTF-8''destek%2",
		"no second quote":  "UTF-8'destek",
		"bad language tag": "UTF-8't r'destek",
		"invalid UTF-8":    "UTF-8''%C3%28 destek talebi",
		"control char":     "UTF-8''destek%07talebi",
		"bidi override":    encode("destek \u202etalebi 4711"),
		"raw Latin-1 byte": "destek talebi \xfe 4711",
	} {
		rec := a.do(t, http.MethodGet, "/api/v1/reason", nil, map[string]string{httpx.HeaderAccessReason: header})
		body := problemBody(t, rec, http.StatusUnprocessableEntity)
		if !strings.Contains(rec.Body.String(), "header.X-Koro-Access-Reason") {
			t.Errorf("%s: %v, want the header location", name, body)
		}
	}
}

// TestAccessReason_HeaderBound: the raw bound of the header fits 500
// characters of 4-byte UTF-8, percent-encoded, and the tag says so.
func TestAccessReason_HeaderBound(t *testing.T) {
	t.Parallel()
	f, _ := reflect.TypeFor[httpx.AccessReasonHeader]().FieldByName("AccessReason")
	if got := f.Tag.Get("maxLength"); got != strconv.Itoa(httpx.MaxAccessReasonHeader) {
		t.Fatalf("maxLength tag = %s, want %d", got, httpx.MaxAccessReasonHeader)
	}
	if n := len(encode(strings.Repeat("😀", 500))); n != httpx.MaxAccessReasonHeader {
		t.Fatalf("500 emoji encode to %d octets, bound %d", n, httpx.MaxAccessReasonHeader)
	}
	a := reasonApp(t)
	long := strings.Repeat("ş", 500)
	if rec := a.do(t, http.MethodGet, "/api/v1/reason", nil, map[string]string{httpx.HeaderAccessReason: encode(long)}); rec.Code != http.StatusOK {
		t.Fatalf("500 Turkish letters: %d %s", rec.Code, rec.Body.String())
	}
}
