package identity

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/httpx"
	"koro/internal/platform/ratelimit"
)

// Limits of the SMS sends (BE-172, BE-181), applied by the service on top of
// the operation's middleware class. Strict: an unavailable limiter refuses
// (503), nothing is sent. limitSMS applies them from the caller's own
// buckets to the shared ones, so a refused send spends nothing further on:
//
//   - per client IP (sign-in codes have a bucket of their own);
//   - per account;
//   - per number and purpose: the sends of one purpose never spend the
//     bucket of another;
//   - per number a day, for the verification of a number the account does
//     not hold;
//   - the SMS budget of the platform (SMSBudget).
var (
	ruleSMSIP        = ratelimit.Rule{Name: "sms_ip", Rate: 30, Per: time.Hour, Strict: true}
	ruleSMSUser      = ratelimit.Rule{Name: "sms_user", Rate: 5, Per: time.Hour, Strict: true}
	ruleSMSNumber    = ratelimit.Rule{Name: "sms_number", Rate: 5, Per: time.Hour, Strict: true}
	ruleSMSNewNumber = ratelimit.Rule{Name: "sms_new_number", Rate: 3, Per: 24 * time.Hour, Strict: true}
)

// limitSMS applies the SMS send limits to one code of purpose for userID to
// number. A limit answers 429 with its remaining wait; a spent budget 503
// identity.sms_unavailable with its own.
func (s *service) limitSMS(ctx context.Context, userID uuid.UUID, purpose SMSPurpose, number string) error {
	if err := s.allow(ctx, ruleSMSIP, smsIPKey(ctx, purpose)); err != nil {
		return err
	}
	if err := s.allow(ctx, ruleSMSUser, "user:"+userID.String()); err != nil {
		return err
	}
	digest, err := s.phoneDigest(number)
	if err != nil {
		return err
	}
	if err := s.allow(ctx, ruleSMSNumber, string(purpose)+":"+digest); err != nil {
		return err
	}
	if purpose == SMSPhoneVerify {
		cur, found, err := s.phones().Phone(ctx, userID)
		if err != nil {
			return err
		}
		if !found || cur.Number != number {
			if err := s.allow(ctx, ruleSMSNewNumber, "phone:"+digest); err != nil {
				return err
			}
		}
	}
	return s.takeSMSBudget(ctx, purpose)
}

// smsIPKey is the client IP bucket of a send of purpose.
func smsIPKey(ctx context.Context, purpose SMSPurpose) string {
	ip := canonicalIP(httpx.ClientIPFrom(ctx))
	if ip == "" {
		ip = "unknown"
	}
	if purpose == SMSLogin {
		return "login:ip:" + ip
	}
	return "ip:" + ip
}

// SMSBudget is the ceiling of the SMS codes of the whole platform (BE-181;
// SMS_BUDGET_PER_HOUR, SMS_BUDGET_PER_DAY). It sits above the adapters and is
// counted by the shared rate limiter (Deps.Limiter: Redis across the API
// processes), whichever SMS_PROVIDER sends. The limiter refills continuously,
// so a spent budget reopens gradually. smsSignInReservePercent of it is open
// to sign-in codes only. When it is spent, a code is refused with 503
// identity.sms_unavailable and Retry-After; a password reset goes by e-mail
// instead, with its usual answer.
type SMSBudget struct {
	PerHour int
	PerDay  int
}

// DefaultSMSBudget applies when Deps.SMSBudget leaves a value at zero (the
// defaults of the configuration, internal/platform/config/sms.go): the budget
// is never unlimited.
var DefaultSMSBudget = SMSBudget{PerHour: 200, PerDay: 1000}

const (
	// smsSignInReservePercent of the budget stays open to sign-in codes
	// when the other purposes have spent their share.
	smsSignInReservePercent = 20
	smsBudgetKey            = "all"
)

// smsBudgetRules are the limiter rules of an SMSBudget, in the order a send
// spends them.
type smsBudgetRules struct {
	signIn []ratelimit.Rule // the whole budget
	others []ratelimit.Rule // their share first, then the whole budget
}

func newSMSBudgetRules(b SMSBudget) smsBudgetRules {
	if b.PerHour <= 0 {
		b.PerHour = DefaultSMSBudget.PerHour
	}
	if b.PerDay <= 0 {
		b.PerDay = DefaultSMSBudget.PerDay
	}
	share := func(n int) int { return max(1, n-n*smsSignInReservePercent/100) }
	hour := ratelimit.Rule{Name: "sms_budget_hour", Rate: b.PerHour, Per: time.Hour, Strict: true}
	day := ratelimit.Rule{Name: "sms_budget_day", Rate: b.PerDay, Per: 24 * time.Hour, Strict: true}
	return smsBudgetRules{
		signIn: []ratelimit.Rule{hour, day},
		others: []ratelimit.Rule{
			{Name: "sms_budget_general_hour", Rate: share(b.PerHour), Per: time.Hour, Strict: true},
			{Name: "sms_budget_general_day", Rate: share(b.PerDay), Per: 24 * time.Hour, Strict: true},
			hour, day,
		},
	}
}

// takeSMSBudget spends the budget of one code of purpose. An unavailable
// limiter refuses like the other SMS limits.
func (s *service) takeSMSBudget(ctx context.Context, purpose SMSPurpose) error {
	rules := s.smsBudget.others
	if purpose == SMSLogin {
		rules = s.smsBudget.signIn
	}
	for _, rule := range rules {
		res, err := s.limiterAllow(ctx, rule, smsBudgetKey)
		if err != nil {
			return err
		}
		if !res.Allowed {
			s.smsCounts.inc(purpose, SMSOutcomeBudgetRefused)
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "SMS budget spent", slog.String("rule", rule.Name), slog.String("purpose", string(purpose)))
			return ErrSMSUnavailable.WithRetryAfter(res.RetryAfter)
		}
	}
	return nil
}

// The ceiling of the SMS password reset of one account in a rolling day
// (BE-181): the codes sent and the tries made, whichever code they went to.
// Above it the reset of that account goes by e-mail, with the usual answers.
const (
	smsResetWindow      = 24 * time.Hour
	maxSMSResetCodes    = 10
	maxSMSResetAttempts = 15
)

// ruleSMSResetNotice: at most one e-mail a day about the ceiling.
var ruleSMSResetNotice = ratelimit.Rule{Name: "sms_reset_notice", Rate: 1, Per: smsResetWindow}

// smsResetUse is the reset use of userID in the window.
func (s *service) smsResetUse(ctx context.Context, userID uuid.UUID) (SMSCodeUse, error) {
	return s.phones().SMSCodeUsage(ctx, userID, SMSPasswordReset, s.now().Add(-smsResetWindow))
}

// mayOpenSMSReset reports whether one more reset code may go to number: the
// account's ceiling, then the SMS limits. ok=false: the e-mail link instead.
func (s *service) mayOpenSMSReset(ctx context.Context, userID uuid.UUID, number string) (bool, error) {
	use, err := s.smsResetUse(ctx, userID)
	if err != nil {
		return false, err
	}
	if use.Codes >= maxSMSResetCodes || use.Attempts >= maxSMSResetAttempts {
		s.smsCounts.inc(SMSPasswordReset, SMSOutcomeResetClosed)
		s.warnSMSResetLimit(ctx, userID)
		return false, nil
	}
	if err := s.limitSMS(ctx, userID, SMSPasswordReset, number); err != nil {
		return false, nil //nolint:nilerr // the SMS limits (or the limiter) refuse: the e-mail link, with the usual answer
	}
	if use.Codes+1 >= maxSMSResetCodes {
		s.warnSMSResetLimit(ctx, userID)
	}
	return true, nil
}

// warnSMSResetLimit tells the account's holder by e-mail that its SMS reset
// reached the ceiling. Best effort: a limiter failure sends nothing.
func (s *service) warnSMSResetLimit(ctx context.Context, userID uuid.UUID) {
	res, err := s.d.Limiter.Allow(ctx, ruleSMSResetNotice, "user:"+userID.String())
	if err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "SMS reset notice not sent", slog.String("err", err.Error()))
		return
	}
	if res.Allowed {
		s.notifyUser(ctx, userID, NoticeSMSResetLimit)
	}
}

// Outcomes of an SMS code: the outcome label of koro_sms_codes_total{purpose,
// outcome}. Both labels come from fixed sets; the metric carries no number,
// no code and no ID.
const (
	SMSOutcomeSent          = "sent"           // the provider accepted it (worker)
	SMSOutcomeRejected      = "rejected"       // the provider refused it for good (worker)
	SMSOutcomeRetried       = "retried"        // a passing failure, tried again while the code lives (worker)
	SMSOutcomeBudgetRefused = "budget_refused" // the SMS budget refused it (API)
	SMSOutcomeResetClosed   = "reset_closed"   // the account's SMS reset ceiling refused it (API)
)

// SMSCounterSample is one value of koro_sms_codes_total.
type SMSCounterSample struct {
	Purpose, Outcome string
	Value            uint64
}

// smsCounters are the in-process totals behind koro_sms_codes_total (read at
// scrape time; monotonic for the life of the process).
type smsCounters struct {
	mu sync.Mutex
	m  map[[2]string]uint64
}

func newSMSCounters() *smsCounters { return &smsCounters{m: map[[2]string]uint64{}} }

func (c *smsCounters) inc(purpose SMSPurpose, outcome string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[[2]string{string(purpose), outcome}]++
}

func (c *smsCounters) snapshot() []SMSCounterSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SMSCounterSample, 0, len(c.m))
	for k, v := range c.m {
		out = append(out, SMSCounterSample{Purpose: k[0], Outcome: k[1], Value: v})
	}
	slices.SortFunc(out, func(a, b SMSCounterSample) int {
		if c := strings.Compare(a.Purpose, b.Purpose); c != 0 {
			return c
		}
		return strings.Compare(a.Outcome, b.Outcome)
	})
	return out
}

// SMSCounters returns koro_sms_codes_total by purpose and outcome of this
// process (the API and the worker register it on their metrics registries).
func (m Module) SMSCounters() []SMSCounterSample { return m.svc.smsCounts.snapshot() }
