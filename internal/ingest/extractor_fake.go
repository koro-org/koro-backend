package ingest

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"koro/internal/platform/textx"
)

// FakeExtractor is the development and test Extractor
// (INGEST_EXTRACTOR_PROVIDER=fake, or claude without a key): no network, no
// model. It reads dates by pattern, one candidate per line that carries a
// date ("11.11.2026", "2026-11-11", "11 Kasım 2026", "11 KASIM", "Nov 11"),
// the rest of the line (or the line before) as the title and the line(s)
// as the evidence. Deterministic, so the pipeline tests know what it finds;
// crude, so production never relies on it.
type FakeExtractor struct{}

var _ Extractor = FakeExtractor{}

// Name implements Extractor.
func (FakeExtractor) Name() string { return "fake" }

var months = map[string]int{
	"ocak": 1, "şubat": 2, "subat": 2, "mart": 3, "nisan": 4, "mayıs": 5, "mayis": 5, "haziran": 6, "temmuz": 7,
	"ağustos": 8, "agustos": 8, "eylül": 9, "eylul": 9, "ekim": 10, "kasım": 11, "kasim": 11, "aralık": 12,
	"aralik": 12, "january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6, "july": 7,
	"august": 8, "september": 9, "october": 10, "november": 11, "december": 12, "jan": 1, "feb": 2, "mar": 3,
	"apr": 4, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var (
	isoDateRe   = regexp.MustCompile(`\b(20\d{2})-(\d{2})-(\d{2})`)
	dottedRe    = regexp.MustCompile(`\b(\d{1,2})[./](\d{1,2})[./](20\d{2})\b`)
	namedDateRe = regexp.MustCompile(`(?i)\b(\d{1,2})(?:\s*[-–]\s*\d{1,2})*\s+([\p{L}]+)(?:\s+(20\d{2}))?`)
	timeRangeRe = regexp.MustCompile(`\b([01]?\d|2[0-3])[:.]([0-5]\d)(?:\s*[-–]\s*([01]?\d|2[0-3])[:.]([0-5]\d))?`)
)

// Extract implements Extractor.
func (FakeExtractor) Extract(_ context.Context, req ExtractRequest) (Extraction, error) {
	var out Extraction
	for i, d := range req.Docs {
		lines := strings.Split(d.Text, "\n")
		for j, line := range lines {
			c, ok := fakeDate(line, req.Today.Year())
			if !ok {
				continue
			}
			c.Doc, c.Organizer, c.Confidence, c.Category = i, d.Organizer, 0.9, string(CategoryOther)
			title := strings.TrimSpace(timeRangeRe.ReplaceAllString(c.Evidence, ""))
			title = strings.Trim(title, " -–:,/|")
			if len([]rune(title)) < minTitle && j > 0 {
				title = strings.TrimSpace(lines[j-1])
				c.Evidence = lines[j-1] + "\n" + line
			} else {
				c.Evidence = line
			}
			c.Title = title
			if m := timeRangeRe.FindStringSubmatch(line); m != nil {
				c.StartTime = m[1] + ":" + m[2]
				if m[3] != "" {
					c.EndTime = m[3] + ":" + m[4]
				}
			}
			out.Candidates = append(out.Candidates, c)
		}
	}
	return out, nil
}

// fakeDate finds a date in line; Evidence carries the line without the date.
func fakeDate(line string, year int) (Candidate, bool) {
	pad := func(n int) string { return strconv.Itoa(n/10) + strconv.Itoa(n%10) }
	if m := isoDateRe.FindStringSubmatchIndex(line); m != nil {
		return Candidate{StartDate: line[m[2]:m[3]] + "-" + line[m[4]:m[5]] + "-" + line[m[6]:m[7]], YearStated: true,
			Evidence: line[:m[0]] + line[m[1]:]}, true
	}
	if m := dottedRe.FindStringSubmatchIndex(line); m != nil {
		d, _ := strconv.Atoi(line[m[2]:m[3]])
		mo, _ := strconv.Atoi(line[m[4]:m[5]])
		if mo >= 1 && mo <= 12 && d >= 1 && d <= 31 {
			return Candidate{StartDate: line[m[6]:m[7]] + "-" + pad(mo) + "-" + pad(d), YearStated: true,
				Evidence: line[:m[0]] + line[m[1]:]}, true
		}
	}
	for _, m := range namedDateRe.FindAllStringSubmatchIndex(line, -1) {
		mo, ok := months[textx.LowerTR(line[m[4]:m[5]])]
		if !ok {
			continue
		}
		d, _ := strconv.Atoi(line[m[2]:m[3]])
		if d < 1 || d > 31 {
			continue
		}
		c := Candidate{Evidence: line[:m[0]] + line[m[1]:]}
		y := year
		if m[6] >= 0 {
			y, _ = strconv.Atoi(line[m[6]:m[7]])
			c.YearStated = true
		}
		c.StartDate = strconv.Itoa(y) + "-" + pad(mo) + "-" + pad(d)
		return c, true
	}
	return Candidate{}, false
}
