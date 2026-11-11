package identity

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	"koro/internal/audit"
	"koro/internal/notify"
	"koro/internal/platform/applink"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/ratelimit"
)

// New sign-in alert (BE-209, Q-64): a sign-in from a device or a network the
// account has not used tells its holder by e-mail, in the app and by push
// (identity.security_new_sign_in, preference security: it cannot be
// switched off on any channel, 02 §6.5).
//
// Recognition reads only what the sessions table already keeps (02 §5.2:
// client kind, IP and User-Agent, until 30 days after a session ended,
// retention session_expired) and compares it reduced the way
// listMySecurityEvents shows it (audit.UserAgentFamily, audit.IPPrefix): no
// new personal data, no fingerprint, no device cookie. A sign-in is
// recognized when, among the account's newest signInHistory session rows,
// one had the same device — the client kind and the browser or app family
// — and one had the same network — the /24 (IPv4) or /48 (IPv6) of the
// address. Both count: the family alone is the User-Agent the caller sends
// (an attacker who signs in with the Koro app or Chrome, as most holders
// do, would never be noticed); the network is the one fact an attacker
// cannot pick.
//
// Noise:
//   - the first sign-in of an account (no last_login_at: the one after the
//     registration) raises nothing; a dormant account whose sessions left
//     the table does alert, since nothing recognizes the device;
//   - a device and a network once seen stay known while their sessions are
//     in the table, so the same device family on a known network never
//     alerts again;
//   - at most newSignInAlerts alerts per account in a rolling day
//     (ruleNewSignInAlert); the limiter failing alerts (fail open);
//   - a sign-in with a second step (TOTP, recovery code, SMS) alerts once,
//     when the session opens, never at the password step.
//
// Takeover: the event is written in the sign-in's own transaction
// (identity.new_sign_in_detected, mechanism O), so the session and its
// alert commit together; nothing the new session does afterwards (log out,
// revoke the holder's sessions, change the password or the address) can
// recall it, and the worker sends it whatever the session's state by then.
// The e-mail goes to the address on the account when the worker runs: an
// address change needs the new address's token first and tells the old one
// (identity.security_notice). Known limits: a sign-in from the holder's own
// network with the holder's device family is not noticed (the price of
// keeping no device identifier), and a push reaches only devices whose
// session is still open when it goes out (BE-182); e-mail and the in-app
// notification always stay.
const (
	// SendNewSignInAlertTaskName is the subscriber of
	// identity.new_sign_in_detected (critical queue: a takeover alert races
	// the takeover).
	SendNewSignInAlertTaskName = "identity.send_new_sign_in_alert"

	newSignInTemplate = "identity.security_new_sign_in"
	newSignInKind     = "new_sign_in" // the notice kind in logs
	signInHistory     = 100           // session rows a sign-in is compared with
	newSignInAlerts   = 3             // alerts per account in a rolling day
)

// ruleNewSignInAlert caps the alerts of one account (key "user:<id>").
var ruleNewSignInAlert = ratelimit.Rule{Name: "new_sign_in_alert", Rate: newSignInAlerts, Per: 24 * time.Hour}

// signInOrigin is what a sign-in is recognized by: no address, no
// User-Agent, only their reductions.
type signInOrigin struct {
	device  string // client kind + browser or app family
	network string // IP prefix ("" when the address is unknown)
}

func originOf(s Session) signInOrigin {
	return signInOrigin{device: string(s.ClientKind) + "|" + audit.UserAgentFamily(s.UserAgent), network: audit.IPPrefix(s.IP)}
}

// newSignIn reports whether the session sess that cur (locked in the
// transaction carried by ctx) is about to open raises a new sign-in alert.
// It runs before the session is inserted, under the user's row lock, so two
// concurrent sign-ins from one new device alert once.
func (s *service) newSignIn(ctx context.Context, cur User, sess Session) (bool, error) {
	if cur.LastLoginAt == nil {
		return false, nil // the account's first sign-in
	}
	history, err := s.sessions().RecentSessions(ctx, cur.ID, signInHistory)
	if err != nil {
		return false, err
	}
	o := originOf(sess)
	device, network := false, false
	for _, h := range history {
		ho := originOf(h)
		device, network = device || ho.device == o.device, network || ho.network == o.network
		if device && network {
			return false, nil
		}
	}
	res, err := s.d.Limiter.Allow(ctx, ruleNewSignInAlert, "user:"+cur.ID.String())
	switch {
	case err != nil:
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "new sign-in alert limit unavailable: alerting", slog.String("user_id", cur.ID.String()),
			slog.String("err", err.Error()))
	case !res.Allowed:
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "new sign-in alert held back by the daily limit", slog.String("user_id", cur.ID.String()))
		return false, nil
	}
	return true, nil
}

// emitNewSignIn writes identity.new_sign_in_detected for sess in the
// transaction carried by ctx.
func (s *service) emitNewSignIn(ctx context.Context, sess Session) error {
	return outbox.Emit(ctx, s.d.Outbox, domainevent.IdentityNewSignInDetected{UserID: sess.UserID, SessionID: sess.ID},
		resourceSession, sess.ID)
}

// handleNewSignIn is the identity.send_new_sign_in_alert handler: the
// in-app notification and the push (fixed texts, no data) and the e-mail,
// which names the device family, the client kind and the time of the
// sign-in (never the address or the network). The session's row is read
// for these; a row already gone (erasure) leaves them out, the alert still
// goes. Idempotent: the dedupe keys are the session's.
func (s *service) handleNewSignIn(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.IdentityNewSignInDetected](e)
	if err != nil {
		return queue.Permanent(err)
	}
	u, err := s.users().User(ctx, p.UserID)
	switch {
	case isNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if u.Status == StatusErased || u.Email == "" {
		return nil
	}
	data := map[string]any{
		"name": u.FirstName, "at": e.OccurredAt,
		"link":       applink.Page(s.d.LinkBaseURL, securityLink.String()),
		"reset_link": applink.Page(s.d.LinkBaseURL, string(applink.ResetPassword)),
	}
	sess, err := s.sessions().Session(ctx, p.SessionID)
	switch {
	case err == nil:
		// No User-Agent at all reads as an unknown client, like any other
		// the families do not name.
		data["at"], data["client"] = sess.CreatedAt, string(sess.ClientKind)
		data["device"] = cmp.Or(audit.UserAgentFamily(sess.UserAgent), "Other")
	case !isNotFound(err):
		return err
	}
	key := "identity.new_sign_in:" + p.SessionID.String()
	mail := notify.Message{
		UserID: &u.ID, ToEmail: u.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: newSignInTemplate, Locale: u.Locale, Data: data, DedupeKey: key,
	}
	return s.sendNotice(ctx, newSignInKind, append(alertMessages(u, newSignInTemplate, key), mail))
}
