package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// Outcome is audit_events.outcome.
type Outcome string

// Outcomes.
const (
	Success Outcome = "success"
	Denied  Outcome = "denied"
	Failure Outcome = "failure"
)

// Event is one audit record as a module hands it to Log (01 §4.1).
//
// Action is the authz action name ("events.event.create", 03 §4.1). The
// actor is ActorUserID (a user), else SystemJob (a worker job), else the
// request was anonymous; use By to fill both from an authz.Actor.
// Metadata holds IDs, kinds, counts and flags of the change: never a
// password, token, key or special-category value (ErrSecretMetadata), and
// never a field the reader may not see. Conventional keys: MetaBreakGlass,
// MetaSensitive, MetaSubjectUserID, MetaReauth, MetaTicketRef.
type Event struct {
	Action       string
	Outcome      Outcome
	ActorUserID  *uuid.UUID
	ActorOrgID   *uuid.UUID // org the actor acted for, when it matters
	SystemJob    string     // job name when a worker acts (authz.SystemActor)
	OrgID        *uuid.UUID // affected org: the scope of audit.event.list_org
	ResourceType string
	ResourceID   *uuid.UUID
	Reason       string // break-glass / export justification (≤ 500 characters)
	Metadata     map[string]any
}

// By returns e with the actor fields of a: a user's ID or a system job.
func (e Event) By(a authz.Actor) Event {
	switch a.Kind {
	case authz.KindUser:
		id := a.UserID
		e.ActorUserID, e.SystemJob = &id, ""
	case authz.KindSystem:
		e.ActorUserID, e.SystemJob = nil, a.SystemJob
	default:
		e.ActorUserID, e.SystemJob = nil, ""
	}
	return e
}

// Conventional metadata keys (03 §6.2, §6.3; 03 k4).
const (
	// MetaBreakGlass (bool): a break-glass access (03 §6.3); the reason is
	// Event.Reason. Filterable with adminQueryAuditEvents {break_glass: true}.
	MetaBreakGlass = "break_glass"
	// MetaSensitive (bool): a sensitive club record; audit.event.list_org
	// returns it only to that club's club_admin (03 §5.21).
	MetaSensitive = "sensitive"
	// MetaSubjectUserID (UUID text): the user whose data a break-glass
	// access read; the event then appears in that user's
	// listMySecurityEvents.
	MetaSubjectUserID = "subject_user_id"
	// MetaReauth ("password" | "totp"): the re-authentication kind (03 §6.2).
	MetaReauth = "reauth"
	// MetaTicketRef (text ≤ 100): the support ticket of a break-glass access.
	MetaTicketRef = "ticket_ref"
)

// Limits of an event.
const (
	maxReason        = 500  // audit_events.reason CHECK
	maxUserAgent     = 400  // audit_events.user_agent CHECK
	maxMetadataBytes = 4096 // serialised metadata
	maxMetadataDepth = 4
	maxTicketRef     = 100
	maxTextField     = 200 // resource_type, system_job
)

// actionRe is the audit_events.action CHECK (= authz action names).
var actionRe = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)+$`)

// Record is one audit_events row (02 §2.1). Empty strings are stored as
// NULL. Seq is assigned by the database; the chain fields are set by the
// chain job only.
type Record struct {
	ID           uuid.UUID
	Seq          int64
	OccurredAt   time.Time
	ActorKind    string // user | system | anonymous
	ActorUserID  *uuid.UUID
	ActorOrgID   *uuid.UUID
	SystemJob    string
	Action       string
	Outcome      Outcome
	OrgID        *uuid.UUID
	ResourceType string
	ResourceID   *uuid.UUID
	Reason       string
	IP           string // canonical netip text or ""
	UserAgent    string
	RequestID    string
	Metadata     json.RawMessage // a JSON object, "{}" when empty

	ChainSeq  *int64
	PrevHash  []byte
	RowHash   []byte
	ChainedAt *time.Time
}

// Actor kinds of audit_events.actor_kind.
const (
	actorUser      = "user"
	actorSystem    = "system"
	actorAnonymous = "anonymous"
)

// record validates e and builds its row with the context facts.
func (s *service) record(ctx context.Context, e Event) (Record, error) {
	invalid := func(format string, args ...any) (Record, error) {
		return Record{}, ErrInvalidEvent.Wrap(fmt.Errorf(format, args...))
	}
	switch {
	case !actionRe.MatchString(e.Action):
		return invalid("action %q is not <module>.<entity>.<verb>", e.Action)
	case e.Outcome != Success && e.Outcome != Denied && e.Outcome != Failure:
		return invalid("outcome %q", e.Outcome)
	case e.ActorUserID != nil && e.SystemJob != "":
		return invalid("an event has a user or a system actor, not both")
	case e.ActorUserID != nil && *e.ActorUserID == uuid.Nil():
		return invalid("nil actor user ID")
	case utf8.RuneCountInString(e.Reason) > maxReason:
		return invalid("reason longer than %d characters", maxReason)
	case len(e.ResourceType) > maxTextField || len(e.SystemJob) > maxTextField:
		return invalid("resource type or system job too long")
	case e.ResourceID != nil && e.ResourceType == "":
		return invalid("resource ID without a resource type")
	}
	meta, err := encodeMetadata(e.Metadata)
	if err != nil {
		return Record{}, err
	}
	r := Record{
		ID: uuid.NewV7(), OccurredAt: s.now(), ActorUserID: e.ActorUserID, ActorOrgID: e.ActorOrgID,
		SystemJob: e.SystemJob, Action: e.Action, Outcome: e.Outcome, OrgID: e.OrgID,
		ResourceType: e.ResourceType, ResourceID: e.ResourceID, Reason: e.Reason, Metadata: meta,
		IP: canonicalIP(httpx.ClientIPFrom(ctx)), UserAgent: cut(httpx.UserAgentFrom(ctx), maxUserAgent),
		RequestID: obs.RequestID(ctx),
	}
	switch {
	case r.ActorUserID != nil:
		r.ActorKind = actorUser
	case r.SystemJob != "":
		r.ActorKind = actorSystem
	default:
		r.ActorKind = actorAnonymous
	}
	if r.RequestID == "" && obs.TaskID(ctx) != "" {
		r.RequestID = "task:" + obs.TaskID(ctx)
	}
	return r, nil
}

// canonicalIP returns the canonical text of an IP address ("" if s is not
// one): the form PostgreSQL's host(inet) prints for a host address.
func canonicalIP(s string) string {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return a.Unmap().WithZone("").String()
}

func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ------------------------------------------------------------ metadata

// secretKeys are metadata keys that name a credential; a key equal to one
// or ending in "_<one>" is rejected ("reset_token", "client_secret").
// Keys that only refer to a credential ("token_id", "reauth") are fine.
var secretKeys = []string{
	"password", "passwd", "pwd", "passphrase", "secret", "token", "tokens", "api_key", "apikey",
	"authorization", "cookie", "set_cookie", "pepper", "private_key", "signing_key", "otp",
	"totp", "totp_code", "otp_code", "mfa_code", "recovery_code", "recovery_codes", "pin", "cvv",
	"card_number", "credential", "credentials", "session_secret",
}

// specialKeys name special-category personal data (KVKK md. 6: race,
// ethnicity, political opinion, philosophical belief, religion, sect,
// dress, association/foundation/union membership, health, sexual life,
// criminal record, security measures, biometric and genetic data).
var specialKeys = []string{
	"race", "ethnicity", "ethnic_origin", "political_opinion", "political_view", "philosophical_belief",
	"belief", "religion", "religious_belief", "sect", "dress", "attire", "association_membership",
	"foundation_membership", "union_membership", "trade_union", "health", "health_data", "diagnosis",
	"disability", "blood_type", "sexual_life", "sexual_orientation", "criminal_record", "conviction",
	"security_measure", "biometric", "biometric_data", "genetic", "genetic_data",
}

// secretValues are value shapes of credentials.
var secretValues = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=\-]{8,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]*`), // JWT
	regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api_key)\s*[=:]\s*\S+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\$argon2(id|i|d)\$`), // password hash
	regexp.MustCompile(`(?i)(__Host-)?koro_session=\S+`),
}

func keyMatches(k string, words []string) bool {
	k = strings.ReplaceAll(strings.ToLower(k), "-", "_")
	for _, w := range words {
		if k == w || strings.HasSuffix(k, "_"+w) {
			return true
		}
	}
	return false
}

// encodeMetadata validates m and returns its JSON object.
func encodeMetadata(m map[string]any) (json.RawMessage, error) {
	if len(m) == 0 {
		return json.RawMessage(`{}`), nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, ErrInvalidEvent.Wrap(fmt.Errorf("metadata is not JSON: %w", err))
	}
	if len(raw) > maxMetadataBytes {
		return nil, ErrInvalidEvent.Wrap(fmt.Errorf("metadata larger than %d bytes", maxMetadataBytes))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil, ErrInvalidEvent.Wrap(fmt.Errorf("metadata: %w", err))
	}
	if err := checkMetadata(v, "", 1); err != nil {
		return nil, err
	}
	if err := checkConventions(v); err != nil {
		return nil, err
	}
	return raw, nil
}

// checkMetadata walks the decoded metadata: no credential or
// special-category key, no credential-shaped string, bounded depth.
func checkMetadata(v any, path string, depth int) error {
	if depth > maxMetadataDepth {
		return ErrInvalidEvent.Wrap(fmt.Errorf("metadata nested deeper than %d levels", maxMetadataDepth))
	}
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if keyMatches(k, secretKeys) {
				return ErrSecretMetadata.Wrap(fmt.Errorf("metadata key %q names a credential", p))
			}
			if keyMatches(k, specialKeys) {
				return ErrSecretMetadata.Wrap(fmt.Errorf("metadata key %q names special-category personal data", p))
			}
			if err := checkMetadata(val, p, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, val := range x {
			if err := checkMetadata(val, path+"[]", depth+1); err != nil {
				return err
			}
		}
	case string:
		for _, re := range secretValues {
			if re.MatchString(x) {
				return ErrSecretMetadata.Wrap(fmt.Errorf("metadata value at %q looks like a credential", path))
			}
		}
	}
	return nil
}

// checkConventions types the conventional keys.
func checkConventions(m map[string]any) error {
	bad := func(k, want string) error {
		return ErrInvalidEvent.Wrap(fmt.Errorf("metadata %s must be %s", k, want))
	}
	for _, k := range []string{MetaBreakGlass, MetaSensitive} {
		if v, ok := m[k]; ok {
			if _, isBool := v.(bool); !isBool {
				return bad(k, "a boolean")
			}
		}
	}
	if v, ok := m[MetaSubjectUserID]; ok {
		s, _ := v.(string)
		if id, err := uuid.Parse(s); err != nil || id.String() != s {
			return bad(MetaSubjectUserID, "a lower-case UUID")
		}
	}
	if v, ok := m[MetaReauth]; ok && v != "password" && v != "totp" {
		return bad(MetaReauth, `"password" or "totp"`)
	}
	if v, ok := m[MetaTicketRef]; ok {
		s, isStr := v.(string)
		if !isStr || utf8.RuneCountInString(s) > maxTicketRef {
			return bad(MetaTicketRef, "text of at most 100 characters")
		}
	}
	return nil
}
