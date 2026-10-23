package config

import (
	"errors"
	"net/url"
	"strings"
)

// Identity configures the identity module (internal/identity). Owner: BE-014;
// only tickets of the identity module edit this file and the
// "# --- identity ---" block of .env.example (BE-001 R7).
//
// Session lifetimes, token lifetimes and the lockout policy are code
// constants of the design (02 §5.1–5.3, 05 §14.1); only deployment facts
// live here.
type Identity struct {
	// LinkBaseURL is the client origin every e-mail link points to
	// (APP_LINK_BASE_URL, BE-096): verification, password reset, e-mail
	// change, invitations (internal/platform/applink) and the page links
	// of events, outreach and identity notices. On a phone with the Koro app
	// the same https link opens the app (Universal/App Links); otherwise the
	// web page. The worker builds the links, so the API's PUBLIC_BASE_URL
	// is not available there. The token travels in the URL fragment, which
	// browsers never send to a server.
	LinkBaseURL string `env:"APP_LINK_BASE_URL"`
	// LegacyLinkBaseURL is IDENTITY_LINK_BASE_URL, the pre-BE-096 name of
	// APP_LINK_BASE_URL. It is still read so existing deployments keep
	// working; when both are set they must be equal. validate folds it into
	// LinkBaseURL.
	LegacyLinkBaseURL string `env:"IDENTITY_LINK_BASE_URL"`
}

// defaultLinkBaseURL is the local web client (dev).
const defaultLinkBaseURL = "http://localhost:3000"

func (s *Identity) validate(c Common) error {
	cur := strings.TrimRight(strings.TrimSpace(s.LinkBaseURL), "/")
	legacy := strings.TrimRight(strings.TrimSpace(s.LegacyLinkBaseURL), "/")
	switch {
	case cur != "" && legacy != "" && cur != legacy:
		return errors.New("APP_LINK_BASE_URL and its old name IDENTITY_LINK_BASE_URL differ: set only APP_LINK_BASE_URL")
	case cur == "" && legacy != "":
		cur = legacy
	case cur == "":
		cur = defaultLinkBaseURL
	}
	s.LinkBaseURL, s.LegacyLinkBaseURL = cur, ""
	u, err := url.Parse(cur)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(cur, "#"):
		return errors.New("APP_LINK_BASE_URL must be an absolute http(s) URL without user info, query or fragment")
	case c.ProdLike() && u.Scheme != "https":
		return errors.New("APP_LINK_BASE_URL must use https in staging/prod")
	case c.ProdLike() && (strings.EqualFold(u.Hostname(), "localhost") || u.Hostname() == "127.0.0.1"):
		return errors.New("APP_LINK_BASE_URL must not point to localhost in staging/prod")
	}
	return nil
}
