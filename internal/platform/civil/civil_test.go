package civil_test

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"testing"
	"time"

	_ "time/tzdata" // the production binary embeds tzdata too (cmd/koro)

	"koro/internal/platform/civil"
)

func mustDate(t *testing.T, s string) civil.Date {
	t.Helper()
	d, err := civil.Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return d
}

var (
	_ sql.Scanner   = (*civil.Date)(nil)
	_ driver.Valuer = civil.Date{}
)

func TestCivilDate_JSONAndSQLRoundTrip(t *testing.T) {
	t.Parallel()
	type dto struct {
		Birth    civil.Date  `json:"birth"`
		Optional *civil.Date `json:"optional,omitempty"`
		Omitted  civil.Date  `json:"omitted,omitzero"`
	}
	in := dto{Birth: mustDate(t, "2008-02-29")}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"birth":"2008-02-29"}` {
		t.Fatalf("Marshal = %s", b)
	}
	var out dto
	if err := json.Unmarshal([]byte(`{"birth":"2008-02-29","optional":"1999-12-31"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Birth != in.Birth || out.Optional == nil || out.Optional.String() != "1999-12-31" {
		t.Fatalf("Unmarshal = %+v", out)
	}
	if _, err := json.Marshal(dto{}); !errors.Is(err, civil.ErrInvalid) {
		t.Fatalf("Marshal(zero) error = %v, want ErrInvalid", err)
	}
	for _, bad := range []string{`{"birth":"2026-02-29"}`, `{"birth":"2026-1-02"}`, `{"birth":"2026-10-03T00:00:00Z"}`, `{"birth":20261003}`} {
		var x dto
		if err := json.Unmarshal([]byte(bad), &x); err == nil {
			t.Errorf("Unmarshal(%s) succeeded", bad)
		}
	}

	t.Run("sql", func(t *testing.T) {
		t.Parallel()
		d := mustDate(t, "2026-10-03")
		v, err := d.Value()
		if err != nil || v != "2026-10-03" {
			t.Fatalf("Value() = %v, %v", v, err)
		}
		sources := []any{
			"2026-10-03",
			[]byte("2026-10-03"),
			time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), // pgx `date`
		}
		for _, src := range sources {
			var got civil.Date
			if err := got.Scan(src); err != nil || got != d {
				t.Errorf("Scan(%T) = %v, %v", src, got, err)
			}
		}
		var got civil.Date
		for _, src := range []any{nil, 42, "03.10.2026", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)} {
			if err := got.Scan(src); !errors.Is(err, civil.ErrInvalid) {
				t.Errorf("Scan(%#v) error = %v, want ErrInvalid", src, err)
			}
		}
		if _, err := (civil.Date{}).Value(); !errors.Is(err, civil.ErrInvalid) {
			t.Fatalf("zero Value() error = %v", err)
		}
	})
}

func TestCivil_Parse(t *testing.T) {
	t.Parallel()
	ok := []string{"0001-01-01", "1900-01-01", "2000-02-29", "2024-02-29", "9999-12-31"}
	for _, s := range ok {
		if d, err := civil.Parse(s); err != nil || d.String() != s {
			t.Errorf("Parse(%q) = %v, %v", s, d, err)
		}
	}
	bad := []string{"", "0000-01-01", "1900-02-29", "2026-02-29", "2026-13-01", "2026-00-10", "2026-04-31", "2026-10-00",
		"2026-10-3", "26-10-03", "2026/10/03", "+026-10-03", "2026-10-03 ", "２０２６-10-03", "2026-1٠-03"}
	for _, s := range bad {
		if _, err := civil.Parse(s); !errors.Is(err, civil.ErrInvalid) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalid", s, err)
		}
	}
}

func TestCivil_IsMinor_BirthdayBoundary(t *testing.T) {
	t.Parallel()
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		birth string
		at    time.Time
		minor bool
	}{
		// 18th birthday 2026-10-03 starts at 2026-10-02T21:00:00Z (Istanbul is UTC+3).
		{"one second before the 18th birthday (Istanbul)", "2008-10-03", time.Date(2026, 10, 2, 20, 59, 59, 0, time.UTC), true},
		{"first second of the 18th birthday (Istanbul)", "2008-10-03", time.Date(2026, 10, 2, 21, 0, 0, 0, time.UTC), false},
		{"UTC date is still the day before", "2008-10-03", time.Date(2026, 10, 2, 23, 30, 0, 0, time.UTC), false},
		{"day after", "2008-10-03", time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), false},
		{"a year before", "2008-10-03", time.Date(2025, 10, 3, 12, 0, 0, 0, time.UTC), true},
		{"29 Feb birth, 28 Feb of a common year", "2008-02-29", time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC), true},
		{"29 Feb birth, 1 Mar of a common year", "2008-02-29", time.Date(2026, 2, 28, 21, 0, 0, 0, time.UTC), false},
		{"28 Feb birth, 29 Feb of a leap year", "2010-02-28", time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC), false},
		{"1 Mar birth, 29 Feb of a leap year", "2010-03-01", time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC), true},
		{"birth date in the future", "2030-01-01", time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), true},
		{"adult", "1990-05-17", time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := mustDate(t, tt.birth).IsMinor(tt.at); got != tt.minor {
				t.Fatalf("IsMinor(%s at %s) = %v, want %v", tt.birth, tt.at.In(istanbul), got, tt.minor)
			}
		})
	}
	if got := civil.Today(time.Date(2026, 10, 2, 21, 0, 0, 0, time.UTC)); got.String() != "2026-10-03" {
		t.Fatalf("Today = %s, want 2026-10-03", got)
	}
}

func TestCivil_Arithmetic(t *testing.T) {
	t.Parallel()
	d := mustDate(t, "2024-02-28")
	if got := d.AddDays(1).String(); got != "2024-02-29" {
		t.Fatalf("AddDays(1) = %s", got)
	}
	if got := d.AddDays(-365).String(); got != "2023-02-28" {
		t.Fatalf("AddDays(-365) = %s", got)
	}
	a, b := mustDate(t, "2026-01-31"), mustDate(t, "2026-02-01")
	if !a.Before(b) || !b.After(a) || a.Compare(a) != 0 || b.Compare(a) != 1 {
		t.Fatal("Compare/Before/After")
	}
	if got := d.In(time.UTC); !got.Equal(time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("In(UTC) = %v", got)
	}
	if _, err := civil.New(2026, 2, 29); !errors.Is(err, civil.ErrInvalid) {
		t.Fatalf("New(2026-02-29) error = %v", err)
	}
	if mustDate(t, "2008-02-29").AgeOn(mustDate(t, "2026-03-01")) != 18 {
		t.Fatal("AgeOn")
	}
}

func FuzzCivil_Parse(f *testing.F) {
	for _, s := range []string{"2026-10-03", "2024-02-29", "0000-00-00", "9999-12-31", "2026-02-30", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := civil.Parse(s)
		if err != nil {
			return
		}
		if !d.IsValid() || d.String() != s {
			t.Fatalf("Parse(%q) = %v does not round-trip", s, d)
		}
		ref, rerr := time.Parse(time.DateOnly, s)
		if rerr != nil || civil.From(ref) != d {
			t.Fatalf("Parse(%q) = %v, time.Parse = %v, %v", s, d, ref, rerr)
		}
	})
}
