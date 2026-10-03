package identity

import (
	"time"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/ratelimit"
)

// sessionPolicy is the idle and absolute lifetime of a session (02 §5.1,
// 03 §2.2-4, §6.4).
type sessionPolicy struct {
	Idle     time.Duration
	Absolute time.Duration
}

// Session lifetimes. The privileged policy applies to every user who holds
// an MFA role (03 §6.4: every platform role, university_rep, company_admin,
// company_owner), whatever the client.
var (
	policyWeb        = sessionPolicy{Idle: 7 * 24 * time.Hour, Absolute: 30 * 24 * time.Hour}
	policyMobile     = sessionPolicy{Idle: 30 * 24 * time.Hour, Absolute: 90 * 24 * time.Hour}
	policyPrivileged = sessionPolicy{Idle: 30 * time.Minute, Absolute: 12 * time.Hour}
)

// touchEvery bounds the writes of the sliding window: last_seen_at moves at
// most once a minute (02 §5.2).
const touchEvery = time.Minute

func policyFor(kind ClientKind, privileged bool) sessionPolicy {
	switch {
	case privileged:
		return policyPrivileged
	case kind == ClientMobile:
		return policyMobile
	default:
		return policyWeb
	}
}

// privileged reports whether a user holds an MFA role (03 §6.4, k-mfa):
// any platform role in effect, or a university_rep, company_admin or
// company_owner membership (career_staff and recruiter are not).
func privileged(roles []authz.PlatformRole, ms []authz.Membership) bool {
	if len(roles) > 0 {
		return true
	}
	for _, m := range ms {
		switch m.Role {
		case authz.MemberUniversityRep, authz.MemberCompanyAdmin, authz.MemberCompanyOwner:
			return true
		default:
		}
	}
	return false
}

// E-mail token lifetimes (02 §5.3).
const (
	ttlAccountEmail  = 24 * time.Hour
	ttlEmailChange   = 24 * time.Hour
	ttlPasswordReset = 30 * time.Minute
)

func tokenTTL(p TokenPurpose) time.Duration {
	if p == PurposePasswordReset {
		return ttlPasswordReset
	}
	return ttlAccountEmail // account_email, email_change (= ttlEmailChange), edu_email
}

// Account lockout (05 §14.1): from the lockoutThreshold-th consecutive
// failure (login or re-authentication) the account is locked for
// lockoutBase, doubling with every further failure up to lockoutMax. A
// success resets the counter. The lock is per account, never per IP; the
// IP limits (auth_login) come on top.
const (
	lockoutThreshold = 5
	lockoutBase      = time.Minute
	lockoutMax       = time.Hour
	maxFailedLogins  = 10000 // users_failed_login_count_chk
)

// lockFor returns how long the account is locked after its n-th
// consecutive failure (0 below the threshold).
func lockFor(n int) time.Duration {
	if n < lockoutThreshold {
		return 0
	}
	d := lockoutBase
	for i := lockoutThreshold; i < n && d < lockoutMax; i++ {
		d *= 2
	}
	return min(d, lockoutMax)
}

// Rate limit rules the service applies itself (04 §1.9, 06 H-6). They reuse
// the class limits; the keys differ from the middleware's, so they count
// separately.
var (
	ruleAuthEmail     = ratelimit.Rules()[op.RateAuthEmail]         // 3/h per (ip, address) and per address
	rulePasswordReset = ratelimit.Rules()[op.RateAuthPasswordReset] // 5/h per address
	ruleReauth        = ratelimit.Rules()[op.RateReauth]            // 5 failures / 15 min per user
)
