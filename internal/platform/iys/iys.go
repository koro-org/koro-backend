// Package iys is the shared vocabulary of Koro's İYS adapters (İleti
// Yönetim Sistemi; Law 6563 and the Ticari İletişim ve Ticari Elektronik
// İletiler Hakkında Yönetmelik; BE-105): consent types and statuses, the
// consent record Koro registers, the states and changes İYS reports, and
// the errors every adapter maps İYS's and its own codes to.
//
// An adapter is a package of its own, chosen by configuration
// (CONSENT_IYS_PROVIDER, cmd/koro/wire_consent.go):
//
//   - official: İYS's own API (internal/platform/iys/official). Usable live
//     only with direct API access (İLETİ+); a smaller service provider
//     reaches İYS through an authorised integrator.
//   - netgsm: the Netgsm integrator's API (internal/platform/iys/netgsm;
//     BE-110): İYS's answers and the changes recipients give İYS itself
//     arrive by webhook (Delivery), the adapter stores them.
//   - iysfake: the in-memory İYS of tests.
//
// There is no adapter interface here: the consumers declare the small
// interfaces they need (consent: IYSSubmitter, IYSFeedReader,
// IYSStateReader; iyscontract: the same three) and an adapter offers the
// capabilities it has by implementing their methods:
//
//	Name() string; Submit(ctx, Consent) (Receipt, error)            every adapter
//	Feed() Feed; Changes(ctx, cursor string) (ChangePage, error)    a change feed (pulled from İYS or pushed by an integrator)
//	Statuses(ctx, Type, []uuid.UUID) (map[uuid.UUID]State, error)  single-recipient states
//	Receive(ctx, Delivery) (int, error)                            webhook deliveries of a pushing integrator
//
// Rules for every adapter (pinned by internal/platform/iys/iyscontract):
//
//   - It resolves recipient addresses itself (identity, through the
//     resolver wire hands it) and keeps them out of every value, error and
//     log line it returns.
//   - It maps İYS's and its own error codes to the errors of this package:
//     ErrRejected (a retry cannot help), ErrTooLate, ErrThrottled,
//     ErrRecipientGone; any other error is transient.
//   - A refusal İYS already holds, or a refusal of a recipient İYS has no
//     record of, is a success (Receipt.AlreadyHeld), not an error.
//   - Changes lists only what recipients gave İYS itself, never Koro's own
//     submissions; pages pulled from İYS and events an integrator pushed
//     (stored by the adapter) are the same feed for the caller.
package iys

import (
	"errors"
	"regexp"
	"time"
	"uuid"

	"koro/internal/platform/clock"
)

// Type is İYS's consent type: the channel of a consent.
type Type string

// İYS consent types.
const (
	Email   Type = "EPOSTA"
	Message Type = "MESAJ"
	Call    Type = "ARAMA"
)

// Status is an İYS consent status.
type Status string

// İYS consent statuses.
const (
	Approved Status = "ONAY"
	Refused  Status = "RET"
)

// MaxStatusBatch is the largest number of users of one Statuses call;
// an adapter splits further if its İYS route needs it.
const MaxStatusBatch = 100

// Errors every adapter maps to. Adapters wrap them (errors.Is); the texts
// they add never repeat an address or a credential.
var (
	// ErrRejected: İYS (or the integrator) refused the request for good; a
	// retry of the same request cannot help (validation, authorisation,
	// subscription).
	ErrRejected = errors.New("iys: refused permanently")
	// ErrTooLate: İYS registers consents taken outside İYS within three
	// business days only (Yönetmelik m.7/11); an older consent is not valid
	// (m.7/12). Always wrapped together with ErrRejected.
	ErrTooLate = errors.New("iys: the consent is older than three business days and cannot be registered")
	// ErrThrottled: a rate limit of İYS or of the adapter's own budget was
	// reached; try again later.
	ErrThrottled = errors.New("iys: rate limit reached")
	// ErrRecipientGone: the user has no address for the type any more, or
	// an address is no Koro user's.
	ErrRecipientGone = errors.New("iys: recipient address not available")
	// ErrUnauthenticated: a webhook delivery that does not prove it comes
	// from the integrator (secret, source address); nothing was stored.
	ErrUnauthenticated = errors.New("iys: webhook delivery not authenticated")
)

// Delivery is one webhook call of a pushing integrator (consent's
// receiveIYSWebhook): the secret of the webhook URL, the client address the
// API resolved (trusted proxies only) and the raw body. Every part may be
// forged; the adapter checks it before storing anything, and none of it is
// ever logged.
type Delivery struct {
	Token    string
	ClientIP string
	Body     []byte
}

// Feed describes an adapter's change feed.
type Feed struct {
	// Name names the feed: the caller keeps one cursor per feed. A stable
	// lower-case code (ValidFeed), e.g. "iys:700123/700124".
	Name string
	// Window is how far back the feed reaches: a cursor whose horizon is
	// older may have lost changes (İYS's change list keeps seven days).
	// 0: the feed keeps every change since the adapter started receiving.
	Window time.Duration
}

// Consent is one consent change Koro registers.
type Consent struct {
	// Ref is Koro's reference of the submission (logs, idempotency keys of
	// integrators); never personal data.
	Ref    string
	UserID uuid.UUID
	Type   Type
	Status Status
	// Source is İYS's source code of the act ("HS_WEB", "HS_MOBIL",
	// "HS_EPOSTA").
	Source string
	// ConsentDate is when the act happened; İYS keeps seconds in Turkish
	// time.
	ConsentDate time.Time
}

// Receipt is İYS's answer to a Submit.
type Receipt struct {
	// TransactionID is İYS's (or the integrator's) reference of the record;
	// "" when nothing was recorded (AlreadyHeld).
	TransactionID string
	// AlreadyHeld: İYS held the state already (a refusal repeated, a
	// refusal of a recipient İYS has no record of); nothing changed.
	AlreadyHeld bool
}

// State is İYS's record of a recipient for a type.
type State struct {
	Status Status
	// At is the record's consent date (İYS keeps seconds). Zero when the
	// adapter learnt the status only (a bulk read of approvals).
	At time.Time
	// Source is İYS's source code of the record ("IYS_WEB" for İYS's own
	// channels, "HS_WEB" for a brand's); "" when unknown.
	Source string
}

// Change is one approval or refusal a recipient gave İYS itself.
type Change struct {
	UserID uuid.UUID
	Type   Type
	State  State
	// RecordedAt is when İYS recorded the change (creationDate).
	RecordedAt time.Time
}

// ChangePage is one page of the change feed.
type ChangePage struct {
	Changes []Change
	// Cursor is the position after this page, to pass to the next call;
	// the given cursor when nothing new arrived (ValidCursor).
	Cursor string
	// Through: every change the feed recorded before it is in this page or
	// an earlier one (the official change list hides its last hour). Zero:
	// the page holds everything the feed had when it was read (a stored
	// push feed); the caller takes the time of its call.
	Through time.Time
	// More: the feed holds more changes right now; call again.
	More bool
	// Skipped counts the changes left out (not Koro users, another
	// recipient type, an unknown type).
	Skipped int
}

var (
	reFeed   = regexp.MustCompile(`^[a-z0-9][a-z0-9:/._-]{0,99}$`)
	reCursor = regexp.MustCompile(`^[\x21-\x7e]{0,1000}$`)
	reSource = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
)

// ValidFeed reports whether s is a feed name (at most 100 characters of
// [a-z0-9:/._-]).
func ValidFeed(s string) bool { return reFeed.MatchString(s) }

// ValidCursor reports whether s fits a stored cursor: at most 1000
// printable ASCII characters without spaces.
func ValidCursor(s string) bool { return reCursor.MatchString(s) }

// PlainSource returns s when it is a plain İYS source code ("IYS_WEB"),
// else "" (a code from an answer is never trusted as free text).
func PlainSource(s string) string {
	if reSource.MatchString(s) {
		return s
	}
	return ""
}

// ValidType reports whether t is an İYS consent type.
func ValidType(t Type) bool { return t == Email || t == Message || t == Call }

// RegistrationDeadline is the end of the third business day after
// consentDate in Turkish time (Yönetmelik m.7/11, m.4/1-i: weekends are not
// business days). Public holidays are not modelled: the doubles use it to
// refuse older consents as İYS does (H467); İYS's own calendar decides live.
func RegistrationDeadline(consentDate time.Time) time.Time {
	loc := clock.Istanbul()
	d := consentDate.In(loc)
	day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	for n := 0; n < 3; {
		day = day.AddDate(0, 0, 1)
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n++
		}
	}
	return day.AddDate(0, 0, 1)
}
