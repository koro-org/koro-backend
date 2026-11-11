package ingest

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"koro/internal/platform/textx"
)

// The club list adapters of the pilot universities (research §2). Each one
// is deterministic over the structure of one official page (the fixtures
// in testdata/ are trimmed copies of 2026-11-11): names, the university's
// own category label, the club's site, its group address and its profile
// links. Board members, advisors and contact persons on those pages are
// never read: the adapters select the name, category, link and e-mail
// elements only.

// clubParse runs one structural parser over the page.
func clubParse(p Page, read func(root *html.Node, base *url.URL) []ClubCandidate) (Parsed, error) {
	root, err := parseHTML(p.Body)
	if err != nil {
		return Parsed{}, fmt.Errorf("%w: %w", errParse, err)
	}
	clubs := read(root, pageURL(p))
	out := make([]ClubCandidate, 0, len(clubs))
	seen := map[string]int{}
	for _, c := range clubs {
		c.Name = strings.Join(strings.Fields(c.Name), " ")
		if len([]rune(c.Name)) < 2 {
			continue
		}
		key := strings.ToLower(c.Name)
		if i, ok := seen[key]; ok { // a name listed twice: keep what each copy adds
			out[i] = mergeClub(out[i], c)
			continue
		}
		seen[key] = len(out)
		out = append(out, c)
	}
	return Parsed{Clubs: out}, nil
}

func mergeClub(a, b ClubCandidate) ClubCandidate {
	if a.Website == "" {
		a.Website = b.Website
	}
	if a.Email == "" {
		a.Email = b.Email
	}
	if a.SourceCategory == "" {
		a.SourceCategory = b.SourceCategory
	}
	if a.Abbr == "" {
		a.Abbr = b.Abbr
	}
	for k, v := range b.Social {
		if a.Social == nil {
			a.Social = map[string]string{}
		}
		if _, ok := a.Social[k]; !ok {
			a.Social[k] = v
		}
	}
	return a
}

// ytuClubList: yildiz.edu.tr/kampus/kampus-yasami/ogrenci-kulupleri —
// Bootstrap accordion items; the header button is "<Kategori> Kulüpleri —
// N Kulüp", the body an <ol> of names.
type ytuClubList struct{}

func (ytuClubList) Name() AdapterName { return AdapterClubListYTU }
func (ytuClubList) Kind() Kind        { return KindClubList }
func (ytuClubList) Accept() []string  { return acceptHTML }

var countSuffixRe = regexp.MustCompile(`\s*[—–-]\s*\d+\s*\p{L}*\s*$`)

func (ytuClubList) Parse(_ context.Context, _ Source, p Page) (Parsed, error) {
	return clubParse(p, func(root *html.Node, _ *url.URL) []ClubCandidate {
		var out []ClubCandidate
		for _, item := range findAll(root, byClass("accordion-item")) {
			cat := ""
			if b := findFirst(item, byClass("accordion-button")); b != nil {
				cat = countSuffixRe.ReplaceAllString(inlineText(b), "")
			}
			body := findFirst(item, byClass("accordion-body"))
			if body == nil {
				continue
			}
			for _, li := range findAll(body, byAtom(atom.Li)) {
				out = append(out, ClubCandidate{Name: inlineText(li), SourceCategory: cat})
			}
		}
		return out
	})
}

// bounClubList: kultursanat.bogazici.edu.tr/tr/pages/kulupler-ve-topluluklar —
// .club-card items: a.club-name (its href is the club's site when not "#"),
// .club-abbr, .club-email mailto. No category on the page.
type bounClubList struct{}

func (bounClubList) Name() AdapterName { return AdapterClubListBOUN }
func (bounClubList) Kind() Kind        { return KindClubList }
func (bounClubList) Accept() []string  { return acceptHTML }

func (bounClubList) Parse(_ context.Context, _ Source, p Page) (Parsed, error) {
	return clubParse(p, func(root *html.Node, base *url.URL) []ClubCandidate {
		var out []ClubCandidate
		for _, card := range findAll(root, byClass("club-card")) {
			a := findFirst(card, byClass("club-name"))
			if a == nil {
				continue
			}
			c := ClubCandidate{Name: titleCaseTR(inlineText(a))}
			if ab := findFirst(card, byClass("club-abbr")); ab != nil {
				c.Abbr = inlineText(ab)
			}
			c.Website, c.Social = siteOrSocial(base, attr(a, "href"), nil)
			if em := findFirst(card, byClass("club-email")); em != nil {
				c.Email = mailtoOf(em)
			}
			out = append(out, c)
		}
		return out
	})
}

// sabanciClubList: studentlife.sabanciuniv.edu/kulupler[?c=<id>] — Drupal
// view rows (.person-item: h5 > a name, .clubs-social > .clubs_<network> > a).
// The category is the exposed filter's option whose value is the page's
// "c" query parameter (one source per category); without it, none. The
// club detail pages (board members' names and e-mails) are not fetched.
type sabanciClubList struct{}

func (sabanciClubList) Name() AdapterName { return AdapterClubListSU }
func (sabanciClubList) Kind() Kind        { return KindClubList }
func (sabanciClubList) Accept() []string  { return acceptHTML }

func (sabanciClubList) Parse(_ context.Context, src Source, p Page) (Parsed, error) {
	cat := ""
	if u, err := url.Parse(src.URL); err == nil {
		cat = u.Query().Get("c")
	}
	return clubParse(p, func(root *html.Node, base *url.URL) []ClubCandidate {
		label := ""
		if sel := findFirst(root, func(n *html.Node) bool { return n.DataAtom == atom.Select && attr(n, "name") == "c" }); sel != nil && cat != "" {
			for _, o := range findAll(sel, byAtom(atom.Option)) {
				if attr(o, "value") == cat {
					label = inlineText(o)
				}
			}
		}
		var out []ClubCandidate
		for _, item := range findAll(root, byClass("person-item")) {
			h := findFirst(item, byAtom(atom.H5))
			if h == nil {
				continue
			}
			c := ClubCandidate{Name: inlineText(h), SourceCategory: label, Social: map[string]string{}}
			if soc := findFirst(item, byClass("clubs-social")); soc != nil {
				for _, a := range findAll(soc, byAtom(atom.A)) {
					if k, link, ok := socialKey(resolve(base, attr(a, "href"))); ok {
						c.Social[k] = link
					}
				}
			}
			out = append(out, c)
		}
		return out
	})
}

// metuClubList: kim.metu.edu.tr/tr/ogrenci-topluluklari — one flat run of
// paragraphs: an upper-case name ending in "TOPLULUĞU" (an index first,
// then each club with its aims, advisor, room and "Web Sayfası" link). The
// advisor line is never read; only the name and the web link are.
type metuClubList struct{}

func (metuClubList) Name() AdapterName { return AdapterClubListMETU }
func (metuClubList) Kind() Kind        { return KindClubList }
func (metuClubList) Accept() []string  { return acceptHTML }

func (metuClubList) Parse(_ context.Context, _ Source, p Page) (Parsed, error) {
	return clubParse(p, func(root *html.Node, base *url.URL) []ClubCandidate {
		var out []ClubCandidate
		cur := -1
		for _, para := range findAll(mainNode(root), byAtom(atom.P)) {
			text := inlineText(para)
			if isUpperName(text) && strings.Contains(text, "TOPLULU") {
				name, abbr := splitAbbr(text)
				out = append(out, ClubCandidate{Name: titleCaseTR(name), Abbr: abbr})
				cur = len(out) - 1
				continue
			}
			if cur < 0 || !strings.HasPrefix(textx.LowerTR(text), "web sayfası") {
				continue
			}
			for _, a := range findAll(para, byAtom(atom.A)) {
				site, social := siteOrSocial(base, attr(a, "href"), out[cur].Social)
				if site != "" && out[cur].Website == "" {
					out[cur].Website = site
				}
				out[cur].Social = social
			}
		}
		return out
	})
}

// ituClubList: kulupler.itu.edu.tr answered 403 on 2026-11-11 (research §2):
// the page could not be seen, so the adapter is a generic reading of a
// club list (list items, headings and links whose text names a club). Its
// source starts disabled and the 403 shows in the run log.
type ituClubList struct{}

func (ituClubList) Name() AdapterName { return AdapterClubListITU }
func (ituClubList) Kind() Kind        { return KindClubList }
func (ituClubList) Accept() []string  { return acceptHTML }

var clubWordRe = regexp.MustCompile(`(?i)(kulüb[üu]|kulup|toplulu[ğg]u|club|society)\b`)

func (ituClubList) Parse(_ context.Context, _ Source, p Page) (Parsed, error) {
	return clubParse(p, func(root *html.Node, base *url.URL) []ClubCandidate {
		var out []ClubCandidate
		for _, n := range findAll(mainNode(root), func(n *html.Node) bool {
			return n.DataAtom == atom.Li || n.DataAtom == atom.H3 || n.DataAtom == atom.H4 || n.DataAtom == atom.H5
		}) {
			text := inlineText(n)
			if len([]rune(text)) > 120 || !clubWordRe.MatchString(text) {
				continue
			}
			c := ClubCandidate{Name: text}
			if isUpperName(text) {
				c.Name = titleCaseTR(text)
			}
			if a := findFirst(n, byAtom(atom.A)); a != nil {
				c.Website, c.Social = siteOrSocial(base, attr(a, "href"), nil)
			}
			out = append(out, c)
		}
		return out
	})
}

// siteOrSocial sorts a club link: a social profile goes to social (a copy
// of the map), any other absolute http(s) URL is the club's site.
func siteOrSocial(base *url.URL, href string, social map[string]string) (string, map[string]string) {
	out := map[string]string{}
	for k, v := range social {
		out[k] = v
	}
	u := resolve(base, href)
	if u == "" {
		return "", out
	}
	if k, link, ok := socialKey(u); ok {
		out[k] = link
		return "", out
	}
	if base != nil && strings.EqualFold(pageHost(u), base.Hostname()) {
		return "", out // a link back to the list's own site is not the club's
	}
	return u, out
}

func pageHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// mailtoOf returns the first mailto: address under n (lower case).
func mailtoOf(n *html.Node) string {
	for _, a := range findAll(n, byAtom(atom.A)) {
		if h := strings.TrimSpace(attr(a, "href")); strings.HasPrefix(strings.ToLower(h), "mailto:") {
			addr, _, _ := strings.Cut(h[len("mailto:"):], "?")
			return strings.ToLower(strings.TrimSpace(addr))
		}
	}
	return ""
}

// isUpperName reports whether s is written in capitals (letters only
// counted, at least three of them).
func isUpperName(s string) bool {
	letters := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			if unicode.IsLower(r) {
				return false
			}
			letters++
		}
	}
	return letters >= 3
}

var abbrRe = regexp.MustCompile(`\s*\(([^()]{1,30})\)\s*$`)

// splitAbbr splits "ÇAĞDAŞ DANS TOPLULUĞU (ÇDT)" into the name and "ÇDT".
func splitAbbr(s string) (string, string) {
	if m := abbrRe.FindStringSubmatchIndex(s); m != nil {
		return strings.TrimSpace(s[:m[0]]), s[m[2]:m[3]]
	}
	return s, ""
}

// keepUpper are the words a Turkish title case leaves in capitals.
var keepUpper = map[string]bool{"IEEE": true, "ACM": true, "AIESEC": true, "IAESTE": true, "TEDX": true, "AR-GE": true,
	"ODTÜ": true, "İTÜ": true, "YTÜ": true, "BÜ": true, "SU": true, "AEGEE": true, "ESN": true, "BEST": true, "UNICEF": true}

// lowerWords are the Turkish conjunctions written in lower case.
var lowerWords = map[string]bool{"ve": true, "ile": true, "ya": true, "da": true, "de": true, "için": true}

// titleCaseTR turns an upper-case name into title case with the Turkish
// rules ("BİLİM VE TEKNİK TOPLULUĞU" → "Bilim ve Teknik Topluluğu").
func titleCaseTR(s string) string {
	if !isUpperName(s) {
		return s
	}
	words := strings.Fields(s)
	for i, w := range words {
		if keepUpper[w] || strings.HasPrefix(w, "(") {
			continue
		}
		lw := textx.LowerTR(w)
		if i > 0 && lowerWords[lw] {
			words[i] = lw
			continue
		}
		r := []rune(lw)
		first := textx.UpperTR(string(r[0]))
		words[i] = first + string(r[1:])
	}
	return strings.Join(words, " ")
}
