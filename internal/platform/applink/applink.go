// Package applink builds the links that e-mails carry to the Koro clients
// (BE-096, koro-mobile B-03). Every token link has one shape:
//
//	<APP_LINK_BASE_URL><path>#token=<token>
//
// The base is the web origin the clients share (config Identity.LinkBaseURL,
// APP_LINK_BASE_URL). On a phone with the app installed the same https URL
// opens the app (iOS Universal Links, Android App Links: the domain is
// associated with the app through /.well-known files served on that
// origin); without the app it opens the web page of the path.
//
// The token travels in the URL fragment: a browser or a link preview bot
// never sends it to a server, so it stays out of access logs, proxies and
// Referer headers, and a GET of the link consumes nothing. The client reads
// the fragment and posts the token to the confirm endpoint of the path
// (TokenPaths); only that POST uses the token.
//
// The paths are a public contract with the web and mobile clients (04
// §2.1, §2.2): they never change, a new purpose adds a path. The one-click
// unsubscribe link of commercial e-mail is not built here: RFC 8058 posts
// to the link itself, so it carries the token in the path and points to
// the API (CONSENT_UNSUBSCRIBE_BASE_URL, BE-062).
package applink

import "strings"

// TokenPath is the path of a link that carries a single-use e-mail token.
// The type keeps token links on the declared paths.
type TokenPath string

// The token link paths and the endpoint each one's client posts the token
// to (04 §2.1, §2.2).
const (
	// VerifyEmail: account e-mail verification → POST
	// /api/v1/auth/email-verifications/confirm {token}.
	VerifyEmail TokenPath = "/auth/verify-email"
	// ResetPassword: password reset → POST
	// /api/v1/auth/password-resets/confirm {token, new_password}.
	ResetPassword TokenPath = "/auth/reset-password" //nolint:gosec // G101: a URL path, not a credential
	// ConfirmEmailChange: proof of the new address → POST
	// /api/v1/auth/email-change/confirm {token}.
	ConfirmEmailChange TokenPath = "/auth/confirm-email-change"
	// VerifyEdu: .edu.tr student verification → POST
	// /api/v1/auth/edu-verifications/confirm {token}.
	VerifyEdu TokenPath = "/auth/verify-edu"
	// AcceptInvitation: organization invitation → POST
	// /api/v1/invitations/accept {token} (signed in).
	AcceptInvitation TokenPath = "/invitations/accept"
)

// TokenParam is the fragment parameter that holds the token.
const TokenParam = "token"

// TokenPaths returns every token link path, in a fixed order: the paths an
// app association file (apple-app-site-association, assetlinks.json
// targets) claims.
func TokenPaths() []TokenPath {
	return []TokenPath{VerifyEmail, ResetPassword, ConfirmEmailChange, VerifyEdu, AcceptInvitation}
}

// Token returns the link of path carrying tok: base + path + "#token=" + tok.
// base is the validated APP_LINK_BASE_URL (no query or fragment); tok is a
// URL-safe token (base64url of internal/platform/token, or the unpadded
// base32 of an invitation), which needs no escaping.
func Token(base string, path TokenPath, tok string) string {
	return Page(base, string(path)) + "#" + TokenParam + "=" + tok
}

// Page returns the link of a client page without a token (a ticket, a
// settings screen): base + path.
func Page(base, path string) string {
	return strings.TrimRight(base, "/") + path
}
