package ingest

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The extractor contract: what goes to the language model and how its
// answer is checked. The answer is never trusted: every candidate is
// validated in code (validateCandidate) and dropped when its evidence is
// not a quote of the masked page, its date is outside [yesterday, +180
// days] or its title is not a title.

// Document is one masked text of a page (a page, a feed item, a JSON item).
type Document struct {
	// URL is the source_url of the events found in the text.
	URL string
	// Text is the masked visible text (untrusted).
	Text string
	// Organizer is the club the document is known to belong to ("" when
	// the source is a university-wide page).
	Organizer string
}

// ExtractRequest is one extraction call.
type ExtractRequest struct {
	Docs []Document
	// Today is the local date of the run (Europe/Istanbul): the year rule
	// and the date window are relative to it.
	Today time.Time
	// University is the university's name (context only).
	University string
}

// Candidate is one event the extractor claims to have found.
type Candidate struct {
	Doc        int     `json:"doc"`         // index of the document
	Title      string  `json:"title"`       // the event's name, no person
	StartDate  string  `json:"start_date"`  // YYYY-MM-DD
	StartTime  string  `json:"start_time"`  // HH:MM or ""
	EndDate    string  `json:"end_date"`    // YYYY-MM-DD or ""
	EndTime    string  `json:"end_time"`    // HH:MM or ""
	YearStated bool    `json:"year_stated"` // the text writes the year
	Location   string  `json:"location"`    // place text or ""
	Category   string  `json:"category"`    // events category or "other"
	Organizer  string  `json:"organizer"`   // club name as written, or ""
	Evidence   string  `json:"evidence"`    // verbatim quote with the title and the date
	Confidence float64 `json:"confidence"`  // 0–1
}

// Extraction is the extractor's answer.
type Extraction struct {
	Candidates   []Candidate
	InputTokens  int
	OutputTokens int
}

// extractCategories are the categories the extractor may give.
var extractCategories = Categories()

// Validation limits.
const (
	minTitle     = 3
	maxTitle     = 200
	maxLocation  = 200
	maxEvidence  = 500
	minEvidence  = 8
	pastWindow   = 24 * time.Hour       // "dün": an event that started yesterday is still shown
	futureWindow = 180 * 24 * time.Hour // research §4: +180 days
)

// ValidEvent is a candidate that passed validation, in the local time zone.
type ValidEvent struct {
	Doc        int
	Title      string
	StartsAt   time.Time
	EndsAt     *time.Time
	AllDay     bool
	Location   string
	Category   Category
	Organizer  string
	Evidence   string
	Confidence float64
}

// Rejection reasons (logged and counted, never shown).
const (
	rejectDoc      = "doc"
	rejectEvidence = "evidence"
	rejectTitle    = "title"
	rejectDate     = "date"
	rejectWindow   = "window"
	rejectLowConf  = "confidence"
)

// validateCandidate checks one candidate against the documents and returns
// the event or the rejection reason. loc is Europe/Istanbul.
func validateCandidate(c Candidate, docs []Document, today time.Time, loc *time.Location, minConfidence float64) (ValidEvent, string) {
	if c.Doc < 0 || c.Doc >= len(docs) {
		return ValidEvent{}, rejectDoc
	}
	ev := strings.TrimSpace(c.Evidence)
	if utf8.RuneCountInString(ev) < minEvidence || utf8.RuneCountInString(ev) > maxEvidence ||
		!verifyEvidence(ev, docs[c.Doc].Text) {
		return ValidEvent{}, rejectEvidence
	}
	if c.Confidence < minConfidence || c.Confidence > 1 {
		return ValidEvent{}, rejectLowConf
	}
	title := cleanTitle(c.Title)
	if n := utf8.RuneCountInString(title); n < minTitle || n > maxTitle {
		return ValidEvent{}, rejectTitle
	}
	start, err := time.ParseInLocation(time.DateOnly, strings.TrimSpace(c.StartDate), loc)
	if err != nil {
		return ValidEvent{}, rejectDate
	}
	todayLocal := time.Date(today.In(loc).Year(), today.In(loc).Month(), today.In(loc).Day(), 0, 0, 0, 0, loc)
	if !c.YearStated {
		start = nearestFuture(start, todayLocal)
	}
	if !dayInEvidence(start.Day(), ev) {
		return ValidEvent{}, rejectEvidence // the date must come from the quote, not from the model
	}
	allDay := true
	if h, m, ok := clockTime(c.StartTime); ok {
		start, allDay = time.Date(start.Year(), start.Month(), start.Day(), h, m, 0, 0, loc), false
	}
	if start.Before(todayLocal.Add(-pastWindow)) || start.After(todayLocal.Add(futureWindow)) {
		return ValidEvent{}, rejectWindow
	}
	out := ValidEvent{Doc: c.Doc, Title: title, StartsAt: start, AllDay: allDay, Evidence: ev, Confidence: c.Confidence,
		Category: CategoryOther, Organizer: strings.TrimSpace(c.Organizer)}
	if end, ok := endOf(c, start, loc); ok {
		out.EndsAt = &end
	}
	if cat := Category(strings.TrimSpace(c.Category)); cat.Valid() {
		out.Category = cat
	}
	if l := strings.Join(strings.Fields(c.Location), " "); l != "" && !hasMask(l) {
		if utf8.RuneCountInString(l) > maxLocation {
			l = string([]rune(l)[:maxLocation])
		}
		out.Location = l
	}
	if hasMask(out.Organizer) {
		out.Organizer = ""
	}
	return out, ""
}

// nearestFuture applies the year rule: a date written without a year is
// the nearest one not before yesterday (a December page announcing
// "12 Ocak" means next January).
func nearestFuture(d, today time.Time) time.Time {
	d = time.Date(today.Year(), d.Month(), d.Day(), 0, 0, 0, 0, today.Location())
	if d.Before(today.Add(-pastWindow)) {
		d = d.AddDate(1, 0, 0)
	}
	return d
}

// endOf returns the end of a candidate when it has a valid one at or after
// start (end date without time: the end of that day).
func endOf(c Candidate, start time.Time, loc *time.Location) (time.Time, bool) {
	day := start
	if d := strings.TrimSpace(c.EndDate); d != "" {
		p, err := time.ParseInLocation(time.DateOnly, d, loc)
		if err != nil {
			return time.Time{}, false
		}
		// The end keeps the start's year when the year was inferred.
		day = time.Date(start.Year(), p.Month(), p.Day(), 0, 0, 0, 0, loc)
		if c.YearStated {
			day = p
		}
		if day.Before(time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)) {
			day = day.AddDate(1, 0, 0)
		}
	} else if c.EndTime == "" {
		return time.Time{}, false
	}
	end := time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 0, 0, loc)
	if h, m, ok := clockTime(c.EndTime); ok {
		end = time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
	}
	if end.Before(start) || end.Sub(start) > 60*24*time.Hour {
		return time.Time{}, false
	}
	return end, true
}

var clockRe = regexp.MustCompile(`^([01]?\d|2[0-3])[:.]([0-5]\d)$`)

// clockTime parses "HH:MM".
func clockTime(s string) (int, int, bool) {
	m := clockRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return h, mi, true
}

// quoteFold normalises what a model changes in a quote: curly quotes,
// dashes, spaces and letter case.
var quoteFold = strings.NewReplacer("“", `"`, "”", `"`, "„", `"`, "’", "'", "‘", "'", "–", "-", "—", "-", " ", " ")

func foldQuote(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(quoteFold.Replace(s)), " "))
}

// verifyEvidence reports whether evidence is a quote of text (spaces,
// quotes, dashes and case folded).
func verifyEvidence(evidence, text string) bool {
	e := foldQuote(evidence)
	return e != "" && strings.Contains(foldQuote(text), e)
}

// dayInEvidence reports whether the day of month appears in the quote as a
// number of its own ("7", "07", "07.11", "2026-11-07T…").
func dayInEvidence(day int, evidence string) bool {
	re := regexp.MustCompile(fmt.Sprintf(`(^|\D)0?%d(\D|$)`, day))
	return re.MatchString(evidence)
}
