package consent

import (
	"encoding/hex"
	"time"
	"uuid"

	"koro/internal/platform/page"
)

// NoticeDTO is a notice version (04 §2.5). body_sha256 is what a client
// sends back as text_sha256 when the user grants the consent text.
type NoticeDTO struct {
	NoticeVersionID string     `json:"notice_version_id" format:"uuid"`
	Kind            string     `json:"kind" enum:"privacy_notice,consent_text,terms_of_use,cookie_notice"`
	PurposeCode     string     `json:"purpose_code,omitempty" doc:"Consent purpose of a consent_text; absent for every other kind"`
	Audience        string     `json:"audience" enum:"all,student,company,club,university"`
	Locale          string     `json:"locale" enum:"tr-TR,en-US"`
	Version         string     `json:"version" doc:"YYYY.MM.DD or YYYY.MM.DD-N"`
	Title           string     `json:"title"`
	BodyMD          string     `json:"body_md" doc:"Markdown text exactly as shown"`
	BodySHA256      string     `json:"body_sha256" doc:"Lower-case hex SHA-256 of the UTF-8 bytes of body_md"`
	Status          string     `json:"status" enum:"draft,published,retired" doc:"Retired versions stay readable as proof"`
	PublishedAt     *time.Time `json:"published_at,omitempty"`
	RetiredAt       *time.Time `json:"retired_at,omitempty"`
}

func toNoticeDTO(n Notice) NoticeDTO {
	d := NoticeDTO{
		NoticeVersionID: n.ID.String(), Kind: string(n.Kind), Audience: string(n.Audience), Locale: n.Locale,
		Version: n.Version, Title: n.Title, BodyMD: n.BodyMD, BodySHA256: hex.EncodeToString(n.BodySHA256),
		Status: string(n.Status()), PublishedAt: n.PublishedAt, RetiredAt: n.RetiredAt,
	}
	if n.PurposeCode != nil {
		d.PurposeCode = string(*n.PurposeCode)
	}
	return d
}

// NoticeList is the body of listCurrentNotices.
type NoticeList struct {
	Items []NoticeDTO `json:"items"`
}

// ConsentDTO is one consent act of the caller (04 §2.5).
type ConsentDTO struct {
	ConsentID   string    `json:"consent_id" format:"uuid"`
	PurposeCode string    `json:"purpose_code"`
	Action      string    `json:"action" enum:"granted,withdrawn"`
	ScopeID     string    `json:"scope_id,omitempty" format:"uuid" doc:"Event registration of a per-registration purpose"`
	OccurredAt  time.Time `json:"occurred_at"`
}

func toConsentDTO(c Consent) ConsentDTO {
	return ConsentDTO{ConsentID: c.ID.String(), PurposeCode: string(c.PurposeCode), Action: string(c.Action),
		ScopeID: idText(c.ScopeID), OccurredAt: c.OccurredAt}
}

// ConsentStateDTO is the current state of one purpose (and scope).
type ConsentStateDTO struct {
	PurposeCode string    `json:"purpose_code"`
	ScopeID     string    `json:"scope_id,omitempty" format:"uuid"`
	State       string    `json:"state" enum:"granted,withdrawn" doc:"The newest act decides"`
	Since       time.Time `json:"since"`
}

// MyConsentsDTO is the body of listMyConsents.
type MyConsentsDTO struct {
	Current    []ConsentStateDTO `json:"current" doc:"Current state of every purpose the caller ever acted on (all pages)"`
	Items      []ConsentDTO      `json:"items" doc:"Consent history, newest first"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// ConsentProofDTO is one consent act with its proof (adminListUserConsents).
type ConsentProofDTO struct {
	ConsentID       string    `json:"consent_id" format:"uuid"`
	PurposeCode     string    `json:"purpose_code"`
	Action          string    `json:"action" enum:"granted,withdrawn"`
	NoticeVersionID string    `json:"notice_version_id,omitempty" format:"uuid"`
	TextSHA256      string    `json:"text_sha256,omitempty" doc:"Hex SHA-256 of the text the user saw"`
	ScopeType       string    `json:"scope_type" enum:"none,registration"`
	ScopeID         string    `json:"scope_id,omitempty" format:"uuid"`
	Channel         string    `json:"channel" enum:"web,mobile,email_link,admin,import"`
	IP              string    `json:"ip,omitempty"`
	UserAgent       string    `json:"user_agent,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
}

func toProofDTO(c Consent) ConsentProofDTO {
	d := ConsentProofDTO{
		ConsentID: c.ID.String(), PurposeCode: string(c.PurposeCode), Action: string(c.Action),
		NoticeVersionID: idText(c.NoticeVersionID), ScopeType: string(c.ScopeType), ScopeID: idText(c.ScopeID),
		Channel: string(c.Channel), IP: c.IP, UserAgent: c.UserAgent, OccurredAt: c.OccurredAt,
	}
	if len(c.TextSHA256) > 0 {
		d.TextSHA256 = hex.EncodeToString(c.TextSHA256)
	}
	return d
}

// ConsentProofList is the body of adminListUserConsents.
type ConsentProofList struct {
	Items      []ConsentProofDTO `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// PurposeDTO is one consent purpose (adminListConsentPurposes).
type PurposeDTO struct {
	PurposeCode   string    `json:"purpose_code"`
	DescriptionTR string    `json:"description_tr"`
	LegalBasis    string    `json:"legal_basis" enum:"explicit_consent,contract,legal_obligation,legitimate_interest"`
	RequiresIYS   bool      `json:"requires_iys"`
	IYSChannel    string    `json:"iys_channel,omitempty" enum:"email,sms,call"`
	ScopeType     string    `json:"scope_type" enum:"none,registration"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `json:"created_at"`
}

func toPurposeDTO(p PurposeDef) PurposeDTO {
	d := PurposeDTO{PurposeCode: string(p.Code), DescriptionTR: p.DescriptionTR, LegalBasis: string(p.LegalBasis),
		RequiresIYS: p.RequiresIYS, ScopeType: string(p.ScopeType), Active: p.Active, CreatedAt: p.CreatedAt}
	if p.IYSChannel != nil {
		d.IYSChannel = string(*p.IYSChannel)
	}
	return d
}

// PurposeList is the body of adminListConsentPurposes.
type PurposeList struct {
	Items []PurposeDTO `json:"items"`
}

// ReauthDTO is the reauth body member of an R operation (04 §1.11). It is
// never logged; the audit row records only its kind.
type ReauthDTO struct {
	Password string `json:"password,omitempty" maxLength:"1024" doc:"Account password (account without MFA)"`
	TOTPCode string `json:"totp_code,omitempty" pattern:"^[0-9]{6}$" doc:"Current TOTP code (account with MFA; recovery codes are not accepted)"`
}

func (r *ReauthDTO) proof() ReauthProof {
	if r == nil {
		return ReauthProof{}
	}
	return ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
}

func idText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// Sort key of every consent list cursor.
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
