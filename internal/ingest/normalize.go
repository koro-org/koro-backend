package ingest

import (
	"crypto/sha256"
	"net/url"
	"regexp"
	"strings"
	"time"
	"uuid"

	"koro/internal/directory"
	"koro/internal/platform/textx"
)

// Normalisation and deduplication are pure functions (one implementation,
// no external system): not ports (05 §1.3, no needless abstraction).

// categoryRule is the club category a university's own label maps to.
type categoryRule struct {
	cat    directory.Category
	review bool // a label whose clubs often border a special category
}

// categoryMap is the deterministic mapping of the pilot universities'
// labels (folded: textx.Slug words, the "kulüpleri" noise dropped) to
// club categories (research §2). A special category (religious, political,
// identity) is never a target: it is decided by a platform admin (03 k7).
var categoryMap = map[string]categoryRule{
	"bilim ve teknoloji":          {cat: directory.CategoryTechnology},
	"fen ve teknoloji":            {cat: directory.CategoryTechnology},
	"teknoloji":                   {cat: directory.CategoryTechnology},
	"muhendislik":                 {cat: directory.CategoryTechnology},
	"kariyer":                     {cat: directory.CategoryCareer},
	"profesyonel yasam":           {cat: directory.CategoryCareer},
	"girisimcilik":                {cat: directory.CategoryEntrepreneurship},
	"spor":                        {cat: directory.CategorySports},
	"sportif":                     {cat: directory.CategorySports},
	"kultur ve sanat":             {cat: directory.CategoryArts},
	"sanat":                       {cat: directory.CategoryArts},
	"sanat performans ve eglence": {cat: directory.CategoryArts},
	"kultur":                      {cat: directory.CategoryCulture},
	"hobi":                        {cat: directory.CategoryHobby},
	"toplumsal gelisim":           {cat: directory.CategorySocialResponsibility},
	"sosyal sorumluluk":           {cat: directory.CategorySocialResponsibility},
	"akademik":                    {cat: directory.CategoryAcademic},
	"sosyal bilimler":             {cat: directory.CategoryAcademic, review: true},
}

// labelNoise are the words a category label carries around its subject.
var labelNoise = map[string]bool{"kulupleri": true, "kulupler": true, "kulup": true, "topluluklari": true,
	"topluluklar": true, "clubs": true, "club": true}

// foldLabel folds a label for categoryMap.
func foldLabel(s string) string {
	var kept []string
	for _, w := range strings.Split(textx.Slug(s, 0), "-") {
		if w != "" && !labelNoise[w] {
			kept = append(kept, w)
		}
	}
	return strings.Join(kept, " ")
}

// sensitiveStems are the name words that may reveal a special category of
// personal data of the members (KVKK md. 6: religion, political opinion,
// ethnicity, sexual life, health, union): such a club starts "other" and
// waits for a platform admin. Over-inclusive on purpose.
var (
	sensitiveWords = map[string]bool{
		"din": true, "dini": true, "inanc": true, "islam": true, "islami": true, "yahudi": true, "musevi": true,
		"alevi": true, "kurt": true, "ermeni": true, "cerkes": true, "laz": true, "kuir": true, "queer": true,
		"lubunya": true, "parti": true, "sendika": true, "etnik": true, "kimlik": true, "hristiyan": true,
		"budist": true, "gay": true, "lezbiyen": true, "trans": true,
	}
	sensitivePrefixes = []string{"siyas", "politik", "sosyalist", "komunist", "marksist", "kemalist", "ataturkcu",
		"milliyetci", "ulkucu", "muhafazakar", "feminis", "lgbt", "ilahiyat", "musluman", "katolik", "ortodoks",
		"engelli", "hasta", "saglik destek"}
)

// sensitiveName reports whether a club's name hints at a special category.
func sensitiveName(name string) bool {
	slug := textx.Slug(name, 0)
	for _, w := range strings.Split(slug, "-") {
		if sensitiveWords[w] {
			return true
		}
	}
	spaced := strings.ReplaceAll(slug, "-", " ")
	for _, p := range sensitivePrefixes {
		if strings.Contains(" "+spaced, " "+p) {
			return true
		}
	}
	return false
}

// clubCategory maps an entry to its club category and whether a platform
// admin must review it: a sensitive name or an unmapped or "review" label
// gives "other"/review; a mapped label gives its category.
func clubCategory(label, name string) (directory.Category, bool) {
	if sensitiveName(name) || sensitiveName(label) {
		return directory.CategoryOther, true
	}
	r, ok := categoryMap[foldLabel(label)]
	if !ok {
		return directory.CategoryOther, true
	}
	return r.cat, r.review
}

// clubDisplayName is the org name of an entry: its name, with the
// abbreviation in parentheses when the list gives one apart.
func clubDisplayName(c ClubCandidate) string {
	name := strings.Join(strings.Fields(c.Name), " ")
	if abbr := strings.TrimSpace(c.Abbr); abbr != "" && !strings.Contains(name, "(") && len([]rune(abbr)) <= 20 {
		name += " (" + abbr + ")"
	}
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
	}
	return name
}

var mailRe = regexp.MustCompile(`^[a-z0-9._%+-]+@([a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,})$`)

// emailDomain returns the domain of a well-formed address.
func emailDomain(addr string) (string, bool) {
	m := mailRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(addr)))
	if m == nil || len(addr) > 254 {
		return "", false
	}
	return m[1], true
}

// httpsSite returns the club site to put on the org (https only, not a
// social profile), or "".
func httpsSite(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || isSocial(raw) || len(raw) > 500 {
		return ""
	}
	return raw
}

// fingerprint is the dedup key of an event: the club (else the
// university), the local start day and the folded title.
func fingerprint(universityID uuid.UUID, clubOrgID *uuid.UUID, start time.Time, title string) []byte {
	scope := "u:" + universityID.String()
	if clubOrgID != nil {
		scope = "c:" + clubOrgID.String()
	}
	sum := sha256.Sum256([]byte(scope + "|" + start.Format(time.DateOnly) + "|" + textx.Slug(title, 0)))
	return sum[:]
}

// bodyHash is the SHA-256 of a fetched body.
func bodyHash(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
