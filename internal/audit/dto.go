package audit

import (
	"encoding/json"
	"time"
	"uuid"

	"koro/internal/platform/civil"
	"koro/internal/platform/page"
)

// SecurityEventDTO is one item of listMySecurityEvents (04 §2.21).
type SecurityEventDTO struct {
	Action          string     `json:"action" doc:"Audit action (authz action name)"`
	Outcome         string     `json:"outcome" enum:"success,denied,failure"`
	OccurredAt      time.Time  `json:"occurred_at" doc:"When it happened; for sign-in attempts the newest attempt"`
	IPPrefix        string     `json:"ip_prefix,omitempty" doc:"Network of the client (/24 or /48) of the caller's own requests; absent otherwise (staff accesses, sign-in attempts)"`
	UserAgentFamily string     `json:"user_agent_family,omitempty" doc:"Browser or app family, e.g. Chrome, Koro app; absent like ip_prefix"`
	Reason          string     `json:"reason,omitempty" doc:"Reason code of a refusal, e.g. invalid_credentials, locked, invalid_mfa_code, reauth_failed; for a break-glass item (break_glass) the justification the staff member gave, free text to show as is"`
	BreakGlass      bool       `json:"break_glass,omitempty" doc:"A break-glass access to the caller's data by Koro support or platform admins; reason is then their justification (free text, not a code)"`
	Attempts        int        `json:"attempts,omitempty" doc:"Number of refused sign-ins to the caller's account summarised by this item (anonymous or from another account): consecutive attempts with the same reason, each at most 15 minutes before the next"`
	FirstOccurredAt *time.Time `json:"first_occurred_at,omitempty" doc:"Oldest attempt of a sign-in attempt item"`
}

// SecurityEventList is the body of listMySecurityEvents.
type SecurityEventList struct {
	Items      []SecurityEventDTO `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// AuditActorDTO identifies who acted.
type AuditActorDTO struct {
	UserID  *uuid.UUID `json:"user_id,omitempty" format:"uuid" doc:"Absent for system jobs and anonymous requests"`
	Display string     `json:"display,omitempty" doc:"Display name, when known"`
}

// AuditEventDTO is one audit event (04 §2.21). ip and user_agent appear
// only for dpo and platform_admin, metadata only in adminListAuditEvents;
// talent.* events show only that they happened (actor, time, reason).
type AuditEventDTO struct {
	EventID      uuid.UUID       `json:"event_id" format:"uuid" doc:"Stable ID of the record (list key, deep links)"`
	Action       string          `json:"action" doc:"Audit action (authz action name)"`
	Outcome      string          `json:"outcome" enum:"success,denied,failure"`
	Actor        AuditActorDTO   `json:"actor"`
	ResourceType string          `json:"resource_type,omitempty"`
	ResourceID   *uuid.UUID      `json:"resource_id,omitempty" format:"uuid"`
	Reason       string          `json:"reason,omitempty"`
	OccurredAt   time.Time       `json:"occurred_at"`
	IP           string          `json:"ip,omitempty"`
	UserAgent    string          `json:"user_agent,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty" doc:"Event details (IDs, kinds, flags); never secrets"`
}

// AuditEventList is the body of the audit event lists.
type AuditEventList struct {
	Items      []AuditEventDTO `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// VerifyRequest is the body of adminVerifyAuditChain.
type VerifyRequest struct {
	FromDay civil.Date `json:"from_day" format:"date" doc:"First day (Europe/Istanbul)"`
	ToDay   civil.Date `json:"to_day" format:"date" doc:"Last day (Europe/Istanbul), at most 31 days after from_day"`
}

// VerifyDTO is the result of adminVerifyAuditChain.
type VerifyDTO struct {
	OK               bool   `json:"ok"`
	CheckedRows      int64  `json:"checked_rows"`
	FirstBadChainSeq *int64 `json:"first_bad_chain_seq,omitempty" doc:"First chain position that fails verification"`
}

// Sort key of every audit list cursor.
const sortKey = "occurred_at"

func cursorOf(fp string, n *After) string {
	if n == nil {
		return ""
	}
	return page.Next(sortKey, fp, page.TimeVal(n.At), page.UUIDVal(n.ID))
}

func afterOf(cursor, fp string) (*After, error) {
	c, err := page.Decode(cursor, sortKey, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	at, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	id, err := c.UUID(1)
	if err != nil {
		return nil, err
	}
	return &After{At: at, ID: id}, nil
}

func toSecurityEventDTO(e SecurityEvent) SecurityEventDTO {
	out := SecurityEventDTO{Action: e.Action, Outcome: string(e.Outcome), OccurredAt: e.OccurredAt,
		IPPrefix: e.IPPrefix, UserAgentFamily: e.UserAgentFamily, Reason: e.Reason, BreakGlass: e.BreakGlass,
		Attempts: e.Attempts}
	if e.Attempts > 0 {
		first := e.FirstOccurredAt
		out.FirstOccurredAt = &first
	}
	return out
}

func toAuditEventDTO(v View) AuditEventDTO {
	return AuditEventDTO{
		EventID: v.ID, Action: v.Action, Outcome: string(v.Outcome), Actor: AuditActorDTO{UserID: v.ActorUserID, Display: v.ActorDisplay},
		ResourceType: v.ResourceType, ResourceID: v.ResourceID, Reason: v.Reason, OccurredAt: v.OccurredAt,
		IP: v.IP, UserAgent: v.UserAgent, Metadata: v.Metadata,
	}
}

func toAuditEventList(p Page[View], fp string) AuditEventList {
	out := AuditEventList{Items: make([]AuditEventDTO, len(p.Items)), NextCursor: cursorOf(fp, p.Next)}
	for i, v := range p.Items {
		out.Items[i] = toAuditEventDTO(v)
	}
	return out
}
