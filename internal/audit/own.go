package audit

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/page"
)

// listMySecurityEvents (04 §2.21) shows the owner every refused sign-in to
// the account (T-316, BE-078): a wrong password (invalid_credentials), an
// attempt on a locked account (locked), a wrong second factor
// (invalid_mfa_code), whoever made it. The record stays as identity wrote
// it — the actor is the request's (anonymous when nobody was signed in),
// never the owner — and the item shows no network facts of a request the
// owner did not make (like a break-glass access).
//
// A break-glass access to the owner's data (03 §6.3) shows the
// justification the staff member gave (X-Koro-Access-Reason, free text;
// T-326, BE-091), as the support access notification does — to the owner
// only, still without the staff member's network facts or ticket_ref.
//
// Anyone who knows an address can add such rows (auth_login allows 10
// requests a minute per IP and the lockout does not stop the records), so
// a burst must not bury the owner's other events: consecutive attempts
// with the same reason, each at most attemptGap before the next, are one
// item that counts them. One request reads at most ownScanRows rows; a
// longer burst continues on the next page. The cursor is the position of
// the last row read, so no row is shown twice or skipped.
const (
	attemptGap  = 15 * time.Minute
	ownScanRows = 1000
)

// reasonCodeRe matches a machine reason code (invalid_credentials, locked,
// invalid_mfa_code, reauth_failed). Free text — a justification typed by
// a person — is shown in listMySecurityEvents only as the justification
// of a break-glass access to the owner's data (BE-091).
var reasonCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ListOwn implements Service.
func (s *service) ListOwn(ctx context.Context, a authz.Actor, q OwnQuery) (Page[SecurityEvent], error) {
	uid := a.UserID
	if err := authz.Can(ctx, a, authz.AuditEventListOwn, authz.Resource{Type: authz.TypeAuditLog, ID: uid, OwnerUserID: &uid}); err != nil {
		return Page[SecurityEvent]{}, err
	}
	limit := page.ClampLimit(q.Limit)
	items := make([]SecurityEvent, 0, limit)
	var last *After // the position of the last row read
	read := 0
	// A page without sign-in attempts takes one read of limit+1 rows, as
	// any list; the rest of the budget is read only to finish a burst.
	after, want := q.After, limit+1
	for {
		rows, err := s.d.Store.ListOwn(ctx, uid, after, want)
		if err != nil {
			return Page[SecurityEvent]{}, err
		}
		for _, r := range rows {
			if read == ownScanRows { // r is past what one request reads
				return Page[SecurityEvent]{Items: items, Next: last}, nil
			}
			n := len(items)
			switch {
			case n > 0 && items[n-1].absorb(r, uid):
			case n == limit: // r starts the next page
				return Page[SecurityEvent]{Items: items, Next: last}, nil
			default:
				items = append(items, securityEvent(r, uid))
			}
			last, read = &After{At: r.OccurredAt, ID: r.ID}, read+1
		}
		if len(rows) < want { // the list ended
			return Page[SecurityEvent]{Items: items}, nil
		}
		after, want = last, ownScanRows-read+1
	}
}

// securityEvent is what the owner user sees of the row r.
func securityEvent(r Record, user uuid.UUID) SecurityEvent {
	ev := SecurityEvent{ID: r.ID, Action: r.Action, Outcome: r.Outcome, OccurredAt: r.OccurredAt}
	switch {
	case r.ActorUserID != nil && *r.ActorUserID == user: // the user's own request
		ev.IPPrefix, ev.UserAgentFamily, ev.Reason = ipPrefix(r.IP), userAgentFamily(r.UserAgent), reasonCode(r.Reason)
	case signInAttempt(r, user): // a sign-in to the account, anonymous or from another account
		ev.Reason, ev.Attempts, ev.FirstOccurredAt = reasonCode(r.Reason), 1, r.OccurredAt
	case breakGlassAccess(r, user): // staff read the user's data
		ev.BreakGlass, ev.Reason = true, strings.TrimSpace(r.Reason)
	}
	// Anything else shows neither network facts nor a reason.
	return ev
}

// breakGlassAccess reports whether r is a break-glass access to user's
// data (03 §6.3): metadata break_glass = true and subject_user_id = user,
// the rule the stores select by.
func breakGlassAccess(r Record, user uuid.UUID) bool {
	var m struct {
		BreakGlass bool   `json:"break_glass"`
		Subject    string `json:"subject_user_id"`
	}
	if len(r.Metadata) == 0 || json.Unmarshal(r.Metadata, &m) != nil {
		return false
	}
	return m.BreakGlass && m.Subject == user.String()
}

// absorb adds r to ev when both are sign-in attempt items (not the user's
// own requests) with the same reason and r (older: lists are newest first)
// is at most attemptGap before ev's oldest attempt.
func (ev *SecurityEvent) absorb(r Record, user uuid.UUID) bool {
	if ev.Attempts == 0 {
		return false
	}
	x := securityEvent(r, user)
	if x.Attempts == 0 || x.Reason != ev.Reason || ev.FirstOccurredAt.Sub(x.OccurredAt) > attemptGap {
		return false
	}
	ev.Attempts++
	ev.FirstOccurredAt = x.OccurredAt
	return true
}

// signInAttempt reports whether r is a refused sign-in to user's account:
// identity.session.create denied with the account as its resource
// (identity: the password step and the second factor), whoever the actor.
func signInAttempt(r Record, user uuid.UUID) bool {
	return r.Action == string(authz.IdentitySessionCreate) && r.Outcome == Denied && r.ResourceType == authz.TypeUser &&
		r.ResourceID != nil && *r.ResourceID == user
}

// reasonCode returns reason when it is a machine reason code, else "".
func reasonCode(reason string) string {
	if reasonCodeRe.MatchString(reason) {
		return reason
	}
	return ""
}
