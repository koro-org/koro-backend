package ingest

import (
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// The quick import's extractor contract (BE-211; research Aşama 2): a
// club's representative pastes the text of the club's own post and/or
// gives its poster; the extractor fills the fields of a draft event with a
// confidence per field. The answer is a preview the representative checks
// and corrects before the ordinary createEvent; it is never trusted all
// the same: validateDraft drops what is not a title, a date outside the
// window, a place or a summary that still names a person, and — when only
// text was given — a draft whose evidence is not a quote of that text.

// DraftImage is one poster sent to the extractor: re-encoded by files'
// sanitiser (no metadata), at most maxDraftImageBytes, untrusted.
type DraftImage struct {
	MediaType string // image/jpeg, image/png or image/webp
	Data      []byte
}

// DraftRequest is one quick import call.
type DraftRequest struct {
	// Text is the masked post text ("" when only images were given).
	Text string
	// Images are the posters (at most maxDraftImages).
	Images []DraftImage
	// Today is the local date of the call (Europe/Istanbul).
	Today time.Time
	// Club is the club's name (context only).
	Club string
}

// DraftConfidence is the extractor's confidence (0–1) per field.
type DraftConfidence struct {
	Title    float64 `json:"title"`
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	Location float64 `json:"location"`
	Summary  float64 `json:"summary"`
	Category float64 `json:"category"`
}

// DraftCandidate is one event the extractor read from the post.
type DraftCandidate struct {
	Title      string          `json:"title"`
	StartDate  string          `json:"start_date"`  // YYYY-MM-DD or ""
	StartTime  string          `json:"start_time"`  // HH:MM or ""
	EndDate    string          `json:"end_date"`    // YYYY-MM-DD or ""
	EndTime    string          `json:"end_time"`    // HH:MM or ""
	YearStated bool            `json:"year_stated"` // the input writes the year
	Location   string          `json:"location"`    // place text or ""
	Summary    string          `json:"summary"`     // one or two sentences, no person
	Category   string          `json:"category"`    // events category or "other"
	Evidence   string          `json:"evidence"`    // the quote (or the poster's words) with the name and the date
	Confidence DraftConfidence `json:"confidence"`
}

// DraftExtraction is the extractor's answer to a quick import.
type DraftExtraction struct {
	Drafts []DraftCandidate
	// ImagesRead is false when the adapter could not read the images it
	// was given (the fake extractor).
	ImagesRead   bool
	InputTokens  int
	OutputTokens int
}

// Quick import limits.
const (
	// maxDraftTextRunes bounds the pasted text (an Instagram caption is
	// at most 2 200 characters).
	maxDraftTextRunes = 5000
	// maxDraftImages bounds the posters of one call (a carousel post).
	maxDraftImages = 3
	// maxDraftImageBytes bounds one poster: base64 inflates it by 4/3,
	// within the Messages API's per-image limit.
	maxDraftImageBytes = 5 << 20
	// maxDrafts bounds the events of one answer (a weekly programme).
	maxDrafts = 5
	// draftFutureWindow is wider than the pipeline's 180 days: the club
	// announces its own event and checks the date itself.
	draftFutureWindow = 365 * 24 * time.Hour
	// Field limits of events (createEvent: title 160, summary 300,
	// location_name 200).
	maxDraftTitle    = 160
	maxDraftSummary  = 300
	maxDraftLocation = 200
)

// QuickDraft is a validated draft, in the local time zone. Nothing of it is
// stored: it prefills the club's createEvent.
type QuickDraft struct {
	Title    string
	StartsAt *time.Time // nil: no usable date (the club fills it in)
	EndsAt   *time.Time
	AllDay   bool // a date without a time: StartsAt is the day's start
	// Online: the place is "online" (events format online, no location).
	Online   bool
	Location string
	Summary  string
	Category Category
	Evidence string
	// Verified: the evidence is a quote of the pasted text (and a start
	// read from it holds the quote's day); false when the draft was read
	// from a poster.
	Verified   bool
	Confidence DraftConfidence
}

// validateDraft checks one candidate against the masked text; ok is false
// when the draft is dropped. hasImages: posters were sent too, so evidence
// that is not a quote of the text may be the poster's words (unverified,
// kept for the club to check).
func validateDraft(c DraftCandidate, maskedText string, hasImages bool, today time.Time, loc *time.Location) (QuickDraft, bool) {
	title := cleanTitle(maskPersonalData(c.Title))
	if utf8.RuneCountInString(title) < minTitle {
		return QuickDraft{}, false
	}
	ev := clip(strings.TrimSpace(c.Evidence), maxEvidence)
	verified := maskedText != "" && utf8.RuneCountInString(ev) >= minEvidence && verifyEvidence(ev, maskedText)
	if !verified && !hasImages {
		return QuickDraft{}, false // text only: what is not in the text was invented
	}
	d := QuickDraft{
		Title: clip(title, maxDraftTitle), Evidence: maskPersonalData(ev), Category: CategoryOther, Verified: verified,
		Confidence: DraftConfidence{
			Title: unit(c.Confidence.Title), Start: unit(c.Confidence.Start), End: unit(c.Confidence.End),
			Location: unit(c.Confidence.Location), Summary: unit(c.Confidence.Summary), Category: unit(c.Confidence.Category),
		},
	}
	if start, allDay, ok := draftStart(c, ev, verified, today, loc); ok {
		d.StartsAt, d.AllDay = &start, allDay
		if end, ok := endOf(Candidate{EndDate: c.EndDate, EndTime: c.EndTime, YearStated: c.YearStated}, start, loc); ok {
			d.EndsAt = &end
		}
	}
	if d.StartsAt == nil {
		d.Confidence.Start = 0
	}
	if d.EndsAt == nil {
		d.Confidence.End = 0
	}
	if l := strings.Join(strings.Fields(maskPersonalData(c.Location)), " "); l != "" && !hasMask(l) {
		if isOnline(l) {
			d.Online = true
		} else {
			d.Location = clip(l, maxDraftLocation)
		}
	} else {
		d.Confidence.Location = 0
	}
	if s := strings.Join(strings.Fields(maskPersonalData(c.Summary)), " "); s != "" {
		d.Summary = clip(s, maxDraftSummary)
	} else {
		d.Confidence.Summary = 0
	}
	if cat := Category(strings.TrimSpace(c.Category)); cat.Valid() {
		d.Category = cat
	}
	return d, true
}

// draftStart is the start of a candidate: its date (the year rule when the
// year is not written), its time in Europe/Istanbul, within [yesterday,
// +365 days]. A date read from the pasted text must appear in the quote
// (the model does not invent it); a poster's date is the club's to check.
func draftStart(c DraftCandidate, ev string, verified bool, today time.Time, loc *time.Location) (time.Time, bool, bool) {
	start, err := time.ParseInLocation(time.DateOnly, strings.TrimSpace(c.StartDate), loc)
	if err != nil {
		return time.Time{}, false, false
	}
	todayLocal := time.Date(today.In(loc).Year(), today.In(loc).Month(), today.In(loc).Day(), 0, 0, 0, 0, loc)
	if !c.YearStated {
		start = nearestFuture(start, todayLocal)
	}
	if verified && !dayInEvidence(start.Day(), ev) {
		return time.Time{}, false, false
	}
	allDay := true
	if h, m, ok := clockTime(c.StartTime); ok {
		start, allDay = time.Date(start.Year(), start.Month(), start.Day(), h, m, 0, 0, loc), false
	}
	if start.Before(todayLocal.Add(-pastWindow)) || start.After(todayLocal.Add(draftFutureWindow)) {
		return time.Time{}, false, false
	}
	return start, allDay, true
}

// isOnline reports whether a place text means an online event (the
// promoted draft and the quick import alike: events format online).
func isOnline(place string) bool {
	p := strings.TrimSpace(place)
	return strings.EqualFold(p, "online") || strings.EqualFold(p, "çevrim içi") || strings.EqualFold(p, "çevrimiçi")
}

// unit clamps a confidence to [0, 1].
func unit(f float64) float64 {
	switch {
	case f < 0 || math.IsNaN(f):
		return 0
	case f > 1:
		return 1
	}
	return f
}
