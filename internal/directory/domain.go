package directory

import (
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// University e-mail domains (02 §8.2; BE-220).
//
// university_email_domains is the allow-list of student verification: an
// address proves a student of a university only when its domain is one of
// that university's active rows, or a dot-suffix of the address domain
// (UniversityByEmailDomain). Rows come from two reviewed sources only:
// the reference CSV (`koro seed reference`, changed by a pull request) and
// adminPutUniversityEmailDomain (platform admin, MFA, audited). Nothing is
// accepted because of its shape: a .edu.tr name proves nothing by itself
// either, the row does.
//
// Most Turkish universities mail their students under .edu.tr (the
// registry hands those names to higher-education institutions only), but
// not all: Sabancı Üniversitesi uses sabanciuniv.edu, İstanbul Bilgi
// Üniversitesi bilgiedu.net (research in BE-220). So a domain outside
// .edu.tr is allowed too, with two guards against an obvious mistake that
// would turn a whole mail service into one university's students: it is
// not a public suffix (edu.tr, com.tr, github.io) and not under a shared
// mailbox provider (gmail.com, outlook.com, a *.onmicrosoft.com tenant).
// A free ".edu" rule would admit any self-declared school; the row is the
// rule.
const (
	maxDomainLen = 253
	eduTRSuffix  = ".edu.tr"
)

// domainRe is the shape of a stored domain: lower-case ASCII labels and an
// alphabetic top-level label. It mirrors university_email_domains_domain_chk
// (000190) for the in-memory store.
var domainRe = regexp.MustCompile(`^([a-z0-9-]+\.)+[a-z]{2,63}$`)

// sharedMailDomains are mail services anybody can get an address at; a
// domain equal to or under one of them is refused. Not exhaustive: the
// admin's review is the rule, this list catches the obvious slip.
var sharedMailDomains = map[string]bool{
	"163.com": true, "aol.com": true, "gmail.com": true, "gmx.com": true, "gmx.de": true, "gmx.net": true,
	"googlemail.com": true, "hotmail.com": true, "hotmail.com.tr": true, "icloud.com": true, "live.com": true,
	"mac.com": true, "mail.com": true, "mail.ru": true, "me.com": true, "msn.com": true, "mynet.com": true,
	"onmicrosoft.com": true, "outlook.com": true, "outlook.com.tr": true, "pm.me": true, "proton.me": true,
	"protonmail.com": true, "qq.com": true, "windowslive.com": true, "ya.ru": true, "yahoo.com": true,
	"yahoo.com.tr": true, "yandex.com": true, "yandex.com.tr": true, "yandex.ru": true, "ymail.com": true,
	"zoho.com": true,
}

// isEduTR reports whether d (normalized) is under .edu.tr.
func isEduTR(d string) bool { return strings.HasSuffix(d, eduTRSuffix) }

// domainRule returns why d (normalized, normalizeDomain) cannot be an
// e-mail domain of a university, "" when it can. The message never echoes
// the domain.
func domainRule(d string) string {
	if len(d) > maxDomainLen || !domainRe.MatchString(d) {
		return "must be a lower-case domain name such as itu.edu.tr"
	}
	if suffix, _ := publicsuffix.PublicSuffix(d); suffix == d {
		return "must be a domain of the university, not a public suffix such as edu.tr"
	}
	for _, c := range domainCandidates(d) {
		if sharedMailDomains[c] {
			return "must be a domain of the university, not a shared mail service"
		}
	}
	return ""
}
