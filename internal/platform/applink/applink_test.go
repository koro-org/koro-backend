package applink_test

import (
	"net/url"
	"strings"
	"testing"

	"koro/internal/platform/applink"
)

// TestToken_Shape pins the public link contract (04 §2.1): base + fixed
// path + "#token=" + token. The token stays in the fragment, so a GET of
// the link sends it to no server.
func TestToken_Shape(t *testing.T) {
	t.Parallel()
	const tok = "AbC-_0123456789abcdefghijklmnopqrstuvwxyzABC" // base64url
	tests := []struct {
		path applink.TokenPath
		want string
	}{
		{applink.VerifyEmail, "https://app.example.invalid/auth/verify-email#token=" + tok},
		{applink.ResetPassword, "https://app.example.invalid/auth/reset-password#token=" + tok},
		{applink.ConfirmEmailChange, "https://app.example.invalid/auth/confirm-email-change#token=" + tok},
		{applink.VerifyEdu, "https://app.example.invalid/auth/verify-edu#token=" + tok},
		{applink.AcceptInvitation, "https://app.example.invalid/invitations/accept#token=" + tok},
	}
	for _, tt := range tests {
		t.Run(string(tt.path), func(t *testing.T) {
			t.Parallel()
			got := applink.Token("https://app.example.invalid/", tt.path, tok)
			if got != tt.want {
				t.Fatalf("Token = %q, want %q", got, tt.want)
			}
			u, err := url.Parse(got)
			if err != nil || u.Path != string(tt.path) || u.RawQuery != "" || u.Fragment != "token="+tok {
				t.Fatalf("parsed %+v, %v: token must be in the fragment only", u, err)
			}
			if strings.Contains(u.Host+u.Path, tok) {
				t.Fatal("token leaked out of the fragment")
			}
		})
	}
}

// TestToken_BaseWithPath: a base with a path prefix (web app under a
// sub-path) keeps the prefix.
func TestToken_BaseWithPath(t *testing.T) {
	t.Parallel()
	if got := applink.Token("https://example.invalid/koro", applink.VerifyEmail, "T"); got != "https://example.invalid/koro/auth/verify-email#token=T" {
		t.Fatalf("got %q", got)
	}
	if got := applink.Page("https://example.invalid/", "/me/registrations/1"); got != "https://example.invalid/me/registrations/1" {
		t.Fatalf("Page = %q", got)
	}
}

// TestTokenPaths_UniqueAndAbsolute: every token path is listed once, starts
// with "/" and has no query, fragment or trailing slash (the association
// files claim exactly these paths).
func TestTokenPaths_UniqueAndAbsolute(t *testing.T) {
	t.Parallel()
	paths := applink.TokenPaths()
	if len(paths) != 5 {
		t.Fatalf("TokenPaths = %v, want the 5 declared paths", paths)
	}
	seen := map[applink.TokenPath]bool{}
	for _, p := range paths {
		s := string(p)
		if seen[p] || !strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.ContainsAny(s, "?#") {
			t.Errorf("bad or duplicate path %q", s)
		}
		seen[p] = true
	}
}
