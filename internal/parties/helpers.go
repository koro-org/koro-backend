package parties

import (
	"context"
	"errors"
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Field limits of 02 §17.1 (and of the address object, BE-036).
const (
	minTitle      = 2
	maxTitle      = 300
	maxTaxOffice  = 100
	maxEmail      = 254
	maxEInvoice   = 200
	maxLine1      = 200
	maxDistrict   = 100
	maxCity       = 60
	maxNote       = 500
	docTTL        = 5 * time.Minute // 04 §1.14: download URLs live at most 5 minutes
	maskedDigits  = 4               // VKN / MERSİS digits the org roles see
	auditResource = authz.TypeLegalParty
	auditDocument = authz.TypeLegalPartyDocument
)

var (
	phoneRe   = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	postalRe  = regexp.MustCompile(`^[0-9]{5}$`)
	countryRe = regexp.MustCompile(`^[A-Z]{2}$`)
)

// buyerTypes are the legal types of a buyer party: a company, a sole
// proprietorship or an institution that buys under its VKN; an individual
// (no VKN) does not buy for a company.
var buyerTypes = []LegalType{LegalCompany, LegalSoleProp, LegalAssociation, LegalFoundation, LegalPublicBody, LegalOther}

// adminSurface is the authz view of /admin/** (03 §3.2: 403 before reading).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// partyResource is the authz view of a party on the org surface orgID
// (03 §3.5: OrgID = the org of the path; Attrs kind and scope, BE-036).
func partyResource(orgID uuid.UUID, op OrgParty) authz.Resource {
	org := orgID
	return authz.Resource{Type: authz.TypeLegalParty, ID: op.Party.ID, OrgID: &org, Attrs: map[string]string{
		"kind": string(op.Party.Kind), "scope": string(op.Scope),
	}}
}

// orgListResource is the org's collection of parties (listOrgLegalParties).
func orgListResource(orgID uuid.UUID) authz.Resource {
	org := orgID
	return authz.Resource{Type: authz.TypeLegalParty, OrgID: &org, Attrs: map[string]string{
		"kind": "", "scope": authz.PartyScopeList,
	}}
}

// newBuyerResource is a buyer party the company org is about to own.
func newBuyerResource(orgID uuid.UUID) authz.Resource {
	org := orgID
	return authz.Resource{Type: authz.TypeLegalParty, OrgID: &org, Attrs: map[string]string{
		"kind": string(KindBuyerCompany), "scope": authz.PartyScopeOrg,
	}}
}

// documentResource is the authz view of a document for its verification
// (the uploader may not verify it).
func documentResource(d Document) authz.Resource {
	return authz.Resource{Type: authz.TypeLegalPartyDocument, ID: d.ID, Attrs: map[string]string{
		authz.AttrUploadedBy: d.UploadedBy.String(),
	}}
}

// fullTaxIDs reports whether a sees VKN and MERSİS unmasked (finance and
// platform admins; BE-036).
func fullTaxIDs(a authz.Actor) bool {
	return a.HasPlatformRole(authz.PlatformAdmin) || a.HasPlatformRole(authz.PlatformFinanceAdmin)
}

// writableOrg is the org status rule of writes on behalf of an org (03
// §2.3): a closed org takes no change, a suspended one only platform
// staff's (finance and platform admins act for the platform).
func writableOrg(a authz.Actor, o orgs.OrgSummary) error {
	switch o.Status {
	case orgs.StatusClosed:
		return orgs.ErrInvalidState
	case orgs.StatusSuspended:
		if !fullTaxIDs(a) {
			return orgs.ErrOrgSuspended
		}
	}
	return nil
}

// org returns the org of an org-surface request (404 when it is unknown).
func (s *service) org(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error) {
	o, err := s.d.Orgs.Get(ctx, orgID)
	if errors.Is(err, orgs.ErrNotFound) {
		return orgs.OrgSummary{}, ErrNotFound
	}
	return o, err
}

// partyEvent is the audit record of a party write or read by a. orgID is
// the org the request acted for (nil on the admin surface without one).
// Metadata carries field names and states, never a value (05 §11.2).
func partyEvent(a authz.Actor, act authz.Action, orgID *uuid.UUID, p LegalParty, reason string, meta map[string]any) audit.Event {
	id := p.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["party_kind"] = string(p.Kind)
	if orgID == nil {
		orgID = p.OrgID
	}
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: orgID, ResourceType: auditResource, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// ---- input cleaning (every message names the field, never its value)

func normalized(loc, s string) (string, error) {
	n, err := textx.NormalizeText(s)
	if err != nil {
		return "", problem.Field(loc, "contains control or bidi characters")
	}
	return n, nil
}

func cleanTitle(loc, s string) (string, error) {
	n, err := normalized(loc, s)
	if err != nil {
		return "", err
	}
	if c := utf8.RuneCountInString(n); c < minTitle || c > maxTitle {
		return "", problem.Field(loc, "must be 2-300 characters")
	}
	return n, nil
}

// cleanLine normalises an optional single-line field; "" clears it (nil).
func cleanLine(loc, s string, maxRunes int) (*string, error) {
	n, err := normalized(loc, s)
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(n) > maxRunes {
		return nil, problem.Field(loc, "must be at most "+strconv.Itoa(maxRunes)+" characters")
	}
	if n == "" {
		return nil, nil
	}
	return &n, nil
}

// cleanVKN checks a VKN (10 digits with its check digit); "" clears it.
func cleanVKN(loc, s string) (*string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	d, ok := digitsOnly(s)
	if !ok || !ValidVKN(d) {
		return nil, problem.Field(loc, "must be a valid 10-digit tax number (VKN)")
	}
	return &d, nil
}

// cleanTCKN checks a TCKN (11 digits with its two check digits).
func cleanTCKN(loc, s string) (string, error) {
	d, ok := digitsOnly(s)
	if !ok || !ValidTCKN(d) {
		return "", problem.Field(loc, "must be a valid 11-digit national ID (TCKN)")
	}
	return d, nil
}

// cleanMERSIS checks a MERSİS number (16 digits); "" clears it.
func cleanMERSIS(loc, s string) (*string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	d, ok := digitsOnly(s)
	if !ok || !ValidMERSIS(d) {
		return nil, problem.Field(loc, "must be a 16-digit MERSİS number")
	}
	return &d, nil
}

// cleanEmail checks a plain address (no display name); "" clears it.
func cleanEmail(loc, s string) (*string, error) {
	e := textx.NormalizeEmail(s)
	if e == "" {
		return nil, nil
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > maxEmail || !strings.Contains(e[strings.LastIndex(e, "@"):], ".") {
		return nil, problem.Field(loc, "must be an e-mail address")
	}
	return &e, nil
}

// cleanPhone accepts E.164 ("+905321234567") or a Turkish national number
// ("0532 123 45 67" → "+905321234567"); "" clears it.
func cleanPhone(loc, s string) (*string, error) {
	p := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '(', ')', '.':
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if p == "" {
		return nil, nil
	}
	if len(p) == 11 && p[0] == '0' {
		p = "+90" + p[1:]
	}
	if !phoneRe.MatchString(p) {
		return nil, problem.Field(loc, "must be a phone number in +<country><number> form")
	}
	return &p, nil
}

// cleanAddress checks the address object: line1 and city are required,
// postal_code is 5 digits, country an ISO 3166 code (default TR).
func cleanAddress(loc string, a Address) (Address, error) {
	var out Address
	var err error
	field := func(name, v string, maxRunes int, required bool) string {
		if err != nil {
			return ""
		}
		var n string
		if n, err = normalized(loc+"."+name, v); err != nil {
			return ""
		}
		switch c := utf8.RuneCountInString(n); {
		case required && c == 0:
			err = problem.Field(loc+"."+name, "is required")
		case c > maxRunes:
			err = problem.Field(loc+"."+name, "must be at most "+strconv.Itoa(maxRunes)+" characters")
		}
		return n
	}
	out.Line1 = field("line1", a.Line1, maxLine1, true)
	out.District = field("district", a.District, maxDistrict, false)
	out.City = field("city", a.City, maxCity, true)
	out.PostalCode = field("postal_code", a.PostalCode, 5, false)
	out.Country = strings.ToUpper(field("country", a.Country, 2, false))
	if err != nil {
		return Address{}, err
	}
	if out.PostalCode != "" && !postalRe.MatchString(out.PostalCode) {
		return Address{}, problem.Field(loc+".postal_code", "must be 5 digits")
	}
	if out.Country == "" {
		out.Country = "TR"
	}
	if !countryRe.MatchString(out.Country) {
		return Address{}, problem.Field(loc+".country", "must be a two-letter country code")
	}
	return out, nil
}

// cleanNote checks the optional note of an admin decision.
func cleanNote(loc, s string) (string, error) {
	n := strings.TrimSpace(s)
	if utf8.RuneCountInString(n) > maxNote {
		return "", problem.Field(loc, "must be at most 500 characters")
	}
	return n, nil
}

func validLegalType(loc string, t LegalType, allowed []LegalType) error {
	if !t.Valid() || allowed != nil && !slices.Contains(allowed, t) {
		return problem.Field(loc, "is not an allowed legal type here")
	}
	return nil
}

func equalPtr[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
