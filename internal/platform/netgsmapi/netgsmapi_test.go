package netgsmapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"koro/internal/platform/netgsmapi"
)

// TestNetgsmAPI_AccountBase pins the shared base URL and credential rules
// of the Netgsm adapters.
func TestNetgsmAPI_AccountBase(t *testing.T) {
	t.Parallel()
	acct := netgsmapi.Account{Username: "8500000000", Password: "test-password"}
	if b, err := acct.Base(); err != nil || b != netgsmapi.DefaultBaseURL {
		t.Fatalf("default base = %q, %v", b, err)
	}
	acct.BaseURL = " https://proxy.example.test/netgsm/ "
	if b, err := acct.Base(); err != nil || b != "https://proxy.example.test/netgsm" {
		t.Fatalf("prefixed base = %q, %v", b, err)
	}
	for _, bad := range []string{"ftp://api.netgsm.com.tr", "https://u:p@api.netgsm.com.tr", "https://api.netgsm.com.tr?x=1",
		"https://api.netgsm.com.tr#f", "https://", "api.netgsm.com.tr"} {
		a := acct
		a.BaseURL = bad
		if _, err := a.Base(); !errors.Is(err, netgsmapi.ErrBaseURL) {
			t.Errorf("Base(%q) = %v, want ErrBaseURL", bad, err)
		}
	}
	a := acct
	a.Password = ""
	if _, err := a.Base(); !errors.Is(err, netgsmapi.ErrCredentials) {
		t.Errorf("no password: %v", err)
	}
}

// TestNetgsmAPI_ClientNeverFollowsRedirects: the built client stops at a
// redirect (the credentials would follow it) and has a timeout.
func TestNetgsmAPI_ClientNeverFollowsRedirects(t *testing.T) {
	t.Parallel()
	c := netgsmapi.Account{}.Client()
	if c.Timeout != netgsmapi.DefaultTimeout || c.CheckRedirect == nil ||
		!errors.Is(c.CheckRedirect(&http.Request{}, nil), http.ErrUseLastResponse) {
		t.Fatalf("client = %+v", c)
	}
	own := &http.Client{}
	if (netgsmapi.Account{HTTPClient: own}).Client() != own {
		t.Fatal("the given client is not used")
	}
}

// TestNetgsmAPI_AnswerHygiene: Flex reads scalars of any JSON type, SafeCode
// never repeats free text, Scrub masks numbers and control characters.
func TestNetgsmAPI_AnswerHygiene(t *testing.T) {
	t.Parallel()
	var v struct{ A, B, C, D netgsmapi.Flex }
	if err := json.Unmarshal([]byte(`{"A":"00","B":42,"C":{"x":1},"D":null}`), &v); err != nil || v.A != "00" || v.B != "42" || v.C != "" || v.D != "" {
		t.Fatalf("Flex = %+v, %v", v, err)
	}
	if netgsmapi.SafeCode("50") != "50" || netgsmapi.SafeCode("Numara 5550000001 hatali") != "?" {
		t.Fatal("SafeCode")
	}
	s := netgsmapi.Scrub("dial tcp: +90 555 000 00 01\nfailed")
	if strings.Contains(s, "555 000") || strings.Contains(s, "\n") {
		t.Fatalf("Scrub = %q", s)
	}
}

// TestNetgsmAPI_Budget is the sliding window of the adapters' call budgets.
func TestNetgsmAPI_Budget(t *testing.T) {
	t.Parallel()
	b := &netgsmapi.Budget{N: 2, Per: time.Minute}
	t0 := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	if !b.Take(t0) || !b.Take(t0.Add(time.Second)) || b.Take(t0.Add(2*time.Second)) {
		t.Fatal("the window of two")
	}
	if !b.Take(t0.Add(time.Minute)) || b.Take(t0.Add(time.Minute)) {
		t.Fatal("one slot frees after a minute")
	}
	if (&netgsmapi.Budget{}).Take(t0) {
		t.Fatal("the zero budget allows a call")
	}
}
