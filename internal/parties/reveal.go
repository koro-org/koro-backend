package parties

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/iban"
)

// The audited IBAN reveal of the finance surface (BE-129; KVKK data
// minimisation and payment security). No list or detail response carries a
// full IBAN: every one shows iban_masked. A finance or platform admin who
// must type the number at the bank (a payout, a refund, a return) or check
// it against its evidence (a bank account, a collection account) asks for
// that one number, with a justification, and every call is audited like a
// break-glass read (03 §6.3, BE-091): the reason, the field and the masked
// number — never the number — under audit.MetaBreakGlass, so the review of
// adminQueryAuditEvents {break_glass: true} lists it. parties, ledger and
// payments share these pieces (parties is the lowest of the three).

// RevealNote ends the description of every reveal operation.
const RevealNote = "X-Koro-Access-Reason (10–500 characters) is mandatory (422 without it) and every call is audited " +
	"with it like a break-glass read (BE-129); the rate class is break_glass. The answer is never cached: keep the " +
	"number in the component's memory only, for the time it is typed."

// IBANRevealDTO is the body of every reveal operation.
type IBANRevealDTO struct {
	IBAN       string `json:"iban" doc:"The full IBAN, without spaces (TR + 24 digits)"`
	IBANMasked string `json:"iban_masked" doc:"TR12 **** **** **** **** 1234 56"`
}

// RevealOutput is the answer of a reveal operation.
type RevealOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IBANRevealDTO
}

// RevealOut answers a reveal of the full IBAN number (no-store).
func RevealOut(number string) *RevealOutput {
	return &RevealOutput{CacheControl: "no-store", Body: IBANRevealDTO{IBAN: number, IBANMasked: iban.Mask(number)}}
}

// Revealed is one full IBAN a reveal hands out: the audit record's action
// and resource (OrgID: the org the resource belongs to, nil for Koro's
// own) and the response field the number stands for.
type Revealed struct {
	Action       authz.Action
	ResourceType string
	ResourceID   uuid.UUID
	OrgID        *uuid.UUID
	Field        string // iban_masked's full form: iban, iban_snapshot, sender_iban, target_iban
	IBAN         string
}

// RevealIBAN is the end of every reveal, once the caller authorised the
// actor and found the resource: the justification (422 at
// header.X-Koro-Access-Reason without one, httpx.RequireAccessReason) and
// the audit record, written before the number leaves (an audit failure
// fails the request). It returns the number.
func RevealIBAN(ctx context.Context, log AuditLogger, a authz.Actor, r Revealed, reason string) (string, error) {
	reason, err := httpx.RequireAccessReason(reason)
	if err != nil {
		return "", err
	}
	id := r.ResourceID
	e := audit.Event{
		Action: string(r.Action), Outcome: audit.Success, OrgID: r.OrgID, ResourceType: r.ResourceType, ResourceID: &id,
		Reason: reason, Metadata: map[string]any{
			"operation": "reveal_iban", "field": r.Field, "iban_masked": iban.Mask(r.IBAN), audit.MetaBreakGlass: true,
		},
	}.By(a)
	if err := log.Log(ctx, e); err != nil {
		return "", err
	}
	return r.IBAN, nil
}
