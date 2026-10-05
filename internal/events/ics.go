package events

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"
)

// iCalendar files of the ticket e-mails (RFC 5545, iTIP RFC 5546; R-003
// §2.5.6, 01 §6.2):
//
//   - one VEVENT per registration, UID "<event_id>.<registration_id>@koro",
//     so a later update or cancellation replaces the attendee's calendar
//     entry;
//   - METHOD:REQUEST for the ticket and the schedule updates, METHOD:CANCEL
//     (STATUS:CANCELLED) when the registration or the event is cancelled;
//   - SEQUENCE grows with every change: it is the number of seconds from
//     the event row's creation to its last update (updated_at moves with
//     every write of the row, a schedule change included), so a later file
//     never carries a smaller SEQUENCE than an earlier one; a cancellation
//     adds one more;
//   - times are UTC (DTSTART:…Z, valid without a VTIMEZONE); the calendar
//     shows them in the attendee's zone;
//   - the QR token is never in the file (R-003 §2.5.6: calendar providers
//     would store it); no online link either (the attendee opens it in
//     Koro).
const (
	icsProdID   = "-//Koro//Etkinlik Bileti//TR"
	icsMethodRQ = "REQUEST"
	icsMethodCX = "CANCEL"
	icsLineMax  = 75 // octets per line before folding (RFC 5545 §3.1)
)

// icsEvent is one calendar file.
type icsEvent struct {
	Method         string // REQUEST or CANCEL
	EventID        uuid.UUID
	RegistrationID uuid.UUID
	Sequence       int
	Stamp          time.Time
	Start, End     time.Time
	Summary        string
	Location       string
	Description    string
	URL            string // the ticket page in Koro (no token)
	OrganizerName  string
	OrganizerEmail string // ORGANIZER (REQUEST/CANCEL need one, RFC 5546 §3.2); "" leaves it out
	AttendeeEmail  string // the registrant (ATTENDEE, PARTSTAT=ACCEPTED, RSVP=FALSE)
}

// icsUID is the UID of a registration's calendar entry.
func icsUID(eventID, registrationID uuid.UUID) string {
	return eventID.String() + "." + registrationID.String() + "@koro"
}

// icsSequence is the SEQUENCE of an event's current state (see above).
func icsSequence(e Event) int {
	d := e.UpdatedAt.Sub(e.CreatedAt) / time.Second
	switch {
	case d < 0:
		return 0
	case d > math.MaxInt32-1:
		return math.MaxInt32 - 1
	}
	return int(d)
}

// icsTime is a UTC DATE-TIME.
func icsTime(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// icsText escapes a TEXT value (RFC 5545 §3.3.11): backslash, semicolon,
// comma and line breaks; other control characters are dropped.
func icsText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == ';':
			b.WriteString(`\;`)
		case r == ',':
			b.WriteString(`\,`)
		case r == '\n':
			b.WriteString(`\n`)
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
			// \r and the other controls never reach a calendar line
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// icsParam is a parameter value: quoted, without the characters a quoted
// parameter cannot hold (DQUOTE and controls; RFC 5545 §3.1).
func icsParam(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r < 0x20 || r == 0x7f || r == utf8.RuneError {
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// icsAddress keeps an address usable in a mailto: URI (no line breaks,
// spaces or separators of the content line).
func icsAddress(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\r\n\";:,<>") || !strings.Contains(s, "@") {
		return ""
	}
	return s
}

// fold writes one content line, folded at 75 octets without splitting a
// UTF-8 sequence, ended by CRLF.
func fold(b *strings.Builder, line string) {
	n := 0
	for len(line) > 0 {
		limit := icsLineMax
		if n > 0 {
			limit = icsLineMax - 1 // the leading space of a continuation
			b.WriteString("\r\n ")
		}
		if len(line) <= limit {
			b.WriteString(line)
			break
		}
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		b.WriteString(line[:cut])
		line = line[cut:]
		n++
	}
	b.WriteString("\r\n")
}

// buildICS renders the calendar file of e.
func buildICS(e icsEvent) []byte {
	var b strings.Builder
	cancel := e.Method == icsMethodCX
	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:" + icsProdID,
		"CALSCALE:GREGORIAN",
		"METHOD:" + e.Method,
		"BEGIN:VEVENT",
		"UID:" + icsUID(e.EventID, e.RegistrationID),
		"SEQUENCE:" + strconv.Itoa(e.Sequence),
		"DTSTAMP:" + icsTime(e.Stamp),
		"DTSTART:" + icsTime(e.Start),
		"DTEND:" + icsTime(e.End),
		"SUMMARY:" + icsText(e.Summary),
	}
	if e.Location != "" {
		lines = append(lines, "LOCATION:"+icsText(e.Location))
	}
	if e.Description != "" {
		lines = append(lines, "DESCRIPTION:"+icsText(e.Description))
	}
	if e.URL != "" {
		lines = append(lines, "URL:"+e.URL)
	}
	if org := icsAddress(e.OrganizerEmail); org != "" {
		lines = append(lines, "ORGANIZER;CN="+icsParam(e.OrganizerName)+":mailto:"+org)
	}
	if to := icsAddress(e.AttendeeEmail); to != "" {
		lines = append(lines, "ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;RSVP=FALSE:mailto:"+to)
	}
	if cancel {
		lines = append(lines, "STATUS:CANCELLED")
	} else {
		lines = append(lines, "STATUS:CONFIRMED", "TRANSP:OPAQUE")
	}
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")
	for _, ln := range lines {
		fold(&b, ln)
	}
	return []byte(b.String())
}

// icsContentType is the MIME type of a calendar attachment of method m.
func icsContentType(method string) string {
	return "text/calendar; method=" + method + "; charset=utf-8"
}
