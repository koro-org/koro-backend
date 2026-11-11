package ingest

import (
	"regexp"
	"strings"
	"unicode"
)

// Personal data never leaves for the language model (research §3, KVKK
// md. 9: no transfer abroad of what is not sent). Before extraction the
// page text is masked:
//
//   - e-mail addresses and Turkish phone numbers, by pattern (exact);
//   - person names, by a heuristic that favours over-masking: a name after
//     an academic or honorific title (Prof. Dr., Doç., Öğr. Gör., Sayın,
//     Mr. …), the value of a labelled line (Konuşmacı:, Akademik Danışman:,
//     İletişim:, Speaker: …), and the capitalised words before "ile
//     söyleşi/sohbet/buluşma" (a talk with a person). A name written
//     without any of these cues passes; the extractor is told never to put
//     a person in an event, the schema has no person field, and a title
//     that still carries a mask is cleaned (cleanTitle).
//
// The masks are fixed tokens, so an evidence quote taken from the masked
// text still matches it (verifyEvidence).
const (
	maskEmail  = "[e-posta]"
	maskPhone  = "[telefon]"
	maskPerson = "[kişi]"
)

var (
	emailRe = regexp.MustCompile(`(?i)[a-z0-9._%+-]+@[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}`)
	// phoneRe: +90 / 0 prefixed or bare ten-digit Turkish numbers with
	// space, dot or hyphen groups ("0 (212) 359 00 00", "+90 532 123 45 67").
	phoneRe = regexp.MustCompile(`(?:\+\s?90[\s.-]?|\b)(?:0[\s.-]?)?\(?[1-9]\d{2}\)?[\s.-]?\d{3}[\s.-]?\d{2}[\s.-]?\d{2}\b`)
	// titleNameRe: one or more titles, then one to three capitalised words.
	titleNameRe = regexp.MustCompile(`(?:(?:Prof|Doç|Doc|Dr|Öğr|Ogr|Arş|Ars|Gör|Gor|Üyesi|Üye|Üys|Av|Uzm|Sn|Mr|Mrs|Ms|Assoc|Asst|Assist)\.\s*)+` +
		`(?:Sayın\s+)?(?:\p{Lu}[\p{L}'’.-]*\s*){1,3}`)
	sayinRe = regexp.MustCompile(`\b(?:Sayın|Sevgili)\s+(?:\p{Lu}[\p{L}'’-]*\s*){1,3}`)
	// labelRe: a labelled line whose value names a person.
	labelRe = regexp.MustCompile(`(?im)^(\s*(?:akademik danışman|danışman|danisman|konuşmacı(?:lar)?|konusmaci(?:lar)?|konuk(?:lar)?|` +
		`moderatör|moderator|sorumlu|iletişim|İletişim|iletisim|başkan|Başkan|baskan|yönetim kurulu|kurucu|eğitmen|egitmen|` +
		`sunucu|speakers?|guests?|advisors?|contact|president|instructors?|trainers?|host)\s*[:：-])[^\n]*`)
	// talkWithRe: "Ada Lovelace ile Söyleşi".
	talkWithRe = regexp.MustCompile(`(?:\p{Lu}[\p{L}'’.-]*\s+){1,3}(ile|İle)(\s+)(söyleşi|Söyleşi|sohbet|Sohbet|buluşma|Buluşma|imza|İmza)`)
)

// maskPersonalData returns s with e-mail addresses, phone numbers and the
// person names the heuristic finds replaced by fixed masks.
func maskPersonalData(s string) string {
	s = emailRe.ReplaceAllString(s, maskEmail)
	s = phoneRe.ReplaceAllString(s, maskPhone)
	s = labelRe.ReplaceAllString(s, "${1} "+maskPerson)
	s = titleNameRe.ReplaceAllString(s, maskPerson+" ")
	s = sayinRe.ReplaceAllString(s, maskPerson+" ")
	s = talkWithRe.ReplaceAllString(s, maskPerson+" ${1}${2}${3}")
	return s
}

// titleNoise are the connector words a title keeps after a mask is taken
// out of it ("[kişi] ile Söyleşi" → "Söyleşi").
var titleNoise = map[string]bool{"ile": true, "İle": true, "with": true, "ve": true, "and": true, "&": true, "-": true,
	"–": true, ":": true, ",": true}

// cleanTitle removes the masks and their dangling connectors from a title
// and collapses its spaces.
func cleanTitle(s string) string {
	for _, m := range []string{maskEmail, maskPhone, maskPerson} {
		s = strings.ReplaceAll(s, m, " ")
	}
	words := strings.Fields(s)
	for len(words) > 0 && titleNoise[words[0]] {
		words = words[1:]
	}
	for len(words) > 0 && titleNoise[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	return strings.TrimFunc(strings.Join(words, " "), func(r rune) bool { return unicode.IsPunct(r) && r != ')' && r != '"' })
}

// hasMask reports whether s carries a mask token.
func hasMask(s string) bool {
	return strings.Contains(s, maskEmail) || strings.Contains(s, maskPhone) || strings.Contains(s, maskPerson)
}
