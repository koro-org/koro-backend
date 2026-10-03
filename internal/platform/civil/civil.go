// Package civil provides Date, a calendar day without time or time zone
// (02 §0.4, 05 §9.2). It carries PostgreSQL `date` columns (birth date,
// invoice date, value date) so that they never shift across time zones the
// way a time.Time at midnight does.
//
//   - JSON / text: "YYYY-MM-DD" (encoding.TextMarshaler). huma treats the
//     type as a string; DTO fields add the tag `format:"date"`.
//   - SQL: database/sql Scanner and driver.Valuer (GORM, pgx stdlib).
//     Nullable columns use *civil.Date.
//   - The zero Date is "no date": it cannot be marshalled or stored, which
//     catches a forgotten field instead of writing 0000-01-01.
//
// "Today" is a function of an instant and a place: From(clock.Now().In(
// clock.Istanbul())) or Today(now). The package is a leaf: it imports only
// the standard library (it loads Europe/Istanbul itself; the binary embeds
// time/tzdata).
package civil

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrInvalid is returned for malformed or impossible dates.
var ErrInvalid = errors.New("civil: invalid date")

// AdultAge is the age of majority (Turkish Civil Code art. 11).
const AdultAge = 18

// Date is a calendar day in the proleptic Gregorian calendar, years 1–9999.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// New returns the date or ErrInvalid if it does not exist (e.g. 2026-02-29).
func New(year int, month time.Month, day int) (Date, error) {
	d := Date{Year: year, Month: month, Day: day}
	if !d.IsValid() {
		return Date{}, fmt.Errorf("%w: %04d-%02d-%02d", ErrInvalid, year, int(month), day)
	}
	return d, nil
}

// From returns the calendar day of t in t's location. Convert t first:
// From(now.In(loc)).
func From(t time.Time) Date {
	y, m, d := t.Date()
	return Date{Year: y, Month: m, Day: d}
}

// Today returns the current day in Europe/Istanbul at instant now.
func Today(now time.Time) Date { return From(now.In(istanbul())) }

// Parse parses exactly "YYYY-MM-DD".
func Parse(s string) (Date, error) {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return Date{}, fmt.Errorf("%w: want YYYY-MM-DD", ErrInvalid)
	}
	y, ok1 := digits(s[0:4])
	m, ok2 := digits(s[5:7])
	d, ok3 := digits(s[8:10])
	if !ok1 || !ok2 || !ok3 {
		return Date{}, fmt.Errorf("%w: want YYYY-MM-DD", ErrInvalid)
	}
	return New(y, time.Month(m), d)
}

func digits(s string) (int, bool) {
	n := 0
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// IsZero reports whether d is the zero Date (also used by `omitzero`).
func (d Date) IsZero() bool { return d == Date{} }

// IsValid reports whether d is a real day in years 1–9999.
func (d Date) IsValid() bool {
	if d.Year < 1 || d.Year > 9999 || d.Month < time.January || d.Month > time.December || d.Day < 1 {
		return false
	}
	return d.Day <= time.Date(d.Year, d.Month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// String returns "YYYY-MM-DD".
func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day) }

// In returns midnight at the start of d in loc.
func (d Date) In(loc *time.Location) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
}

// AddDays returns d shifted by n days.
func (d Date) AddDays(n int) Date { return From(d.In(time.UTC).AddDate(0, 0, n)) }

// Compare returns -1, 0 or +1.
func (d Date) Compare(o Date) int {
	switch {
	case d.Year != o.Year:
		return cmpInt(d.Year, o.Year)
	case d.Month != o.Month:
		return cmpInt(int(d.Month), int(o.Month))
	default:
		return cmpInt(d.Day, o.Day)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Before reports whether d is before o.
func (d Date) Before(o Date) bool { return d.Compare(o) < 0 }

// After reports whether d is after o.
func (d Date) After(o Date) bool { return d.Compare(o) > 0 }

// AgeOn returns the age in completed years on day `on` of someone born on d.
// The birthday counts from its first moment; someone born on 29 February
// completes a year on 1 March in common years (as 02 §5.1's
// `birth_date > today - 18 years` does in PostgreSQL).
func (d Date) AgeOn(on Date) int {
	age := on.Year - d.Year
	if on.Month < d.Month || on.Month == d.Month && on.Day < d.Day {
		age--
	}
	return age
}

// IsMinor reports whether someone born on d is under AdultAge at instant
// at, judged by the calendar day in Europe/Istanbul (02 §5.1 age gate).
// A birth date after that day also counts as minor.
func (d Date) IsMinor(at time.Time) bool { return d.AgeOn(Today(at)) < AdultAge }

// MarshalText implements encoding.TextMarshaler ("YYYY-MM-DD").
func (d Date) MarshalText() ([]byte, error) {
	if !d.IsValid() {
		return nil, fmt.Errorf("%w: cannot marshal %s", ErrInvalid, d)
	}
	return []byte(d.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Date) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// Value implements driver.Valuer. The date travels as "YYYY-MM-DD" text,
// which PostgreSQL reads as `date` whatever the parameter type inference.
func (d Date) Value() (driver.Value, error) {
	if !d.IsValid() {
		return nil, fmt.Errorf("%w: cannot store %s", ErrInvalid, d)
	}
	return d.String(), nil
}

// Scan implements sql.Scanner. pgx returns `date` as time.Time at UTC
// midnight; text forms are accepted too. NULL is an error: nullable columns
// use *civil.Date.
func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case time.Time:
		*d = From(v)
		if !d.IsValid() {
			return fmt.Errorf("%w: out of range", ErrInvalid)
		}
		return nil
	case string:
		return d.UnmarshalText([]byte(v))
	case []byte:
		return d.UnmarshalText(v)
	case nil:
		return fmt.Errorf("%w: NULL (use *civil.Date)", ErrInvalid)
	default:
		return fmt.Errorf("%w: cannot scan %T", ErrInvalid, src)
	}
}

var istanbul = sync.OnceValue(func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		panic("civil: load Europe/Istanbul: " + err.Error())
	}
	return loc
})
