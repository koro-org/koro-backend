package orgs

import (
	"context"
	"errors"
	"strings"
	"uuid"

	"koro/internal/identity"
	"koro/internal/platform/problem"
	"koro/internal/platform/taxid"
	"koro/internal/platform/textx"
)

// The review facts of a company application (BE-165, Q-33: anyone may
// register a company, a platform admin verifies it): a tax identifier that
// passes its check digits and an e-mail domain the applicant holds an
// address at. Neither blocks the application; the admin sees both
// (tax_id_valid, domain_verified) before adminVerifyOrg.

// taxIdentity is the cleaned tax identifier of a request: its kind and
// the VKN (a TCKN is checked, never kept: 06 H-60).
type taxIdentity struct {
	kind *TaxIDKind
	no   *string
}

// cleanTaxID checks the tax identifier of a request: a VKN (tax_no, 10
// digits) or a sole trader's TCKN (tckn, 11 digits), not both; spaces,
// dots and hyphens are ignored. Empty values give no identifier. The
// number is never echoed in the error (05 §11.2).
func cleanTaxID(vkn, tckn string) (taxIdentity, error) {
	vkn, tckn = strings.TrimSpace(vkn), strings.TrimSpace(tckn)
	switch {
	case vkn != "" && tckn != "":
		return taxIdentity{}, problem.Field("body.tckn", "must be empty when tax_no is given")
	case vkn != "":
		d, ok := taxid.VKN(vkn)
		if !ok {
			return taxIdentity{}, problem.Field("body.tax_no", "must be a valid 10-digit tax number (VKN)")
		}
		k := TaxIDVKN
		return taxIdentity{kind: &k, no: &d}, nil
	case tckn != "":
		if _, ok := taxid.TCKN(tckn); !ok {
			return taxIdentity{}, problem.Field("body.tckn", "must be a valid 11-digit national ID (TCKN)")
		}
		k := TaxIDTCKN
		return taxIdentity{kind: &k}, nil
	}
	return taxIdentity{}, nil
}

// cleanTaxOffice checks the tax office (legal_parties.tax_office's rule);
// "" clears it.
func cleanTaxOffice(s string) (*string, error) {
	return cleanLine("body.tax_office", s, taxid.MaxTaxOffice)
}

// Field messages of ErrTaxIDRequired.
const (
	msgTaxIDRequired     = "a VKN (tax_no) or a sole trader's TCKN (tckn) is required"
	msgTaxOfficeRequired = "is required"
)

// taxIDRequired is ErrTaxIDRequired with a coded field error at each
// missing location (Q-45: the tax identity is required).
func taxIDRequired(missingID, missingOffice bool) error {
	var fe []problem.FieldError
	if missingID {
		fe = append(fe, problem.FieldError{Location: "body.tax_no", Message: msgTaxIDRequired, Code: ErrTaxIDRequired.Code()})
	}
	if missingOffice {
		fe = append(fe, problem.FieldError{Location: "body.tax_office", Message: msgTaxOfficeRequired, Code: ErrTaxIDRequired.Code()})
	}
	return ErrTaxIDRequired.WithFields(fe...)
}

// freeMailDomains are the free and ISP mailbox providers: anyone can hold
// an address there, so none of them is a company's own e-mail domain
// (BE-165). The list is matched exactly; a company's own subdomain of a
// provider does not exist.
var freeMailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true,
	"hotmail.com": true, "hotmail.com.tr": true, "outlook.com": true, "outlook.com.tr": true, "live.com": true,
	"msn.com": true, "windowslive.com": true,
	"yahoo.com": true, "yahoo.com.tr": true, "ymail.com": true, "rocketmail.com": true,
	"icloud.com": true, "me.com": true, "mac.com": true,
	"aol.com": true, "gmx.com": true, "gmx.net": true, "gmx.de": true, "mail.com": true, "zoho.com": true,
	"yandex.com": true, "yandex.com.tr": true, "yandex.ru": true, "ya.ru": true, "mail.ru": true,
	"protonmail.com": true, "proton.me": true, "pm.me": true, "tutanota.com": true, "tuta.io": true,
	"fastmail.com": true, "hey.com": true,
	"mynet.com": true, "superonline.com": true, "ttmail.com": true, "turk.net": true,
}

// freeMailDomain reports whether d (lower case) is a free mailbox provider.
func freeMailDomain(d string) bool { return freeMailDomains[d] }

// addressDomain is the domain of an e-mail address in lower case, "" for
// an address without one.
func addressDomain(email string) string {
	n := textx.NormalizeEmail(email)
	i := strings.LastIndexByte(n, '@')
	if i < 0 {
		return ""
	}
	return n[i+1:]
}

// holdsAddressAt reports whether userID's account address is at domain
// (exactly: a subdomain or a parent domain does not count, so a public
// suffix can never be verified through one of its registrants). The
// account's address is verified: an account signs in only once its
// address is (identity: pending_email cannot sign in), and an address
// change applies only after the new one is confirmed. No user directory
// or an unknown user → false.
func (s *service) holdsAddressAt(ctx context.Context, userID uuid.UUID, domain string) (bool, error) {
	if s.d.Users == nil || domain == "" || freeMailDomain(domain) {
		return false, nil
	}
	c, err := s.d.Users.Contact(ctx, userID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return addressDomain(c.Email) == domain, nil
}
