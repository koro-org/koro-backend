package config

import (
	"errors"
	"fmt"
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
	// LinkBaseURL is the web application origin the e-mail links point to
	// (verification, password reset, e-mail change). The worker builds the
	// links, so the API's PUBLIC_BASE_URL is not available there. The token
	// travels in the URL fragment, which browsers never send to a server.
	LinkBaseURL string `env:"IDENTITY_LINK_BASE_URL" envDefault:"http://localhost:3000"`
}

func (s *Identity) validate(c Common) error {
	u, err := url.Parse(s.LinkBaseURL)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("IDENTITY_LINK_BASE_URL must be an absolute http(s) URL without query or fragment (got %q)", s.LinkBaseURL)
	case c.ProdLike() && u.Scheme != "https":
		return errors.New("IDENTITY_LINK_BASE_URL must use https in staging/prod")
	case c.ProdLike() && (strings.EqualFold(u.Hostname(), "localhost") || u.Hostname() == "127.0.0.1"):
		return errors.New("IDENTITY_LINK_BASE_URL must not point to localhost in staging/prod")
	}
	s.LinkBaseURL = strings.TrimRight(s.LinkBaseURL, "/")
	return nil
}
