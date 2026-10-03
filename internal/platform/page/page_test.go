package page_test

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

func enc(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestPage_CursorRoundTrip(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 3, 9, 15, 30, 123456789, time.FixedZone("TRT", 3*3600))
	id := uuid.MustParse("01929a3c-5e8f-7b6a-9c4d-2e1f0a9b8c7d")
	fp := page.Fingerprint("published", "org-1")

	s := page.Next("starts_at", fp, page.TimeVal(at), page.UUIDVal(id))
	if len(s) > page.MaxCursorLen || strings.ContainsAny(s, "+/=") {
		t.Fatalf("cursor %q is not short base64url", s)
	}
	c, err := page.Decode(s, "starts_at", fp, 2)
	if err != nil {
		t.Fatalf("Decode error = %v", err)
	}
	gotT, err := c.Time(0)
	if err != nil || !gotT.Equal(at) || gotT.Location() != time.UTC {
		t.Fatalf("Time(0) = %v, %v; want %v in UTC", gotT, err, at)
	}
	gotID, err := c.UUID(1)
	if err != nil || gotID != id {
		t.Fatalf("UUID(1) = %v, %v", gotID, err)
	}
	if _, err := c.Time(1); !errors.Is(err, page.ErrInvalidCursor) {
		t.Fatalf("Time(1) on a uuid error = %v", err)
	}
	if _, err := c.UUID(2); !errors.Is(err, page.ErrInvalidCursor) {
		t.Fatalf("UUID(2) out of range error = %v", err)
	}

	first, err := page.Decode("", "starts_at", fp, 2)
	if first != nil || err != nil {
		t.Fatalf("empty cursor = %v, %v; want first page", first, err)
	}

	noFilter := page.Next("id", "", page.UUIDVal(id))
	if c, err := page.Decode(noFilter, "id", "", 1); err != nil || c.Vals[0] != id.String() {
		t.Fatalf("Decode(id cursor) = %+v, %v", c, err)
	}
}

func TestPage_CursorFilterMismatch_Rejected(t *testing.T) {
	t.Parallel()
	fp := page.Fingerprint("published")
	valid := page.Next("starts_at", fp, page.TimeVal(time.Unix(0, 0)), page.UUIDVal(uuid.Nil()))
	tests := []struct {
		name, cursor, sort, fp string
		nvals                  int
	}{
		{"other filter", valid, "starts_at", page.Fingerprint("draft"), 2},
		{"filter dropped", valid, "starts_at", "", 2},
		{"other sort key", valid, "created_at", fp, 2},
		{"value count", valid, "starts_at", fp, 1},
		{"version 2", enc(`{"v":2,"s":"starts_at","k":["a","b"],"f":"` + fp + `"}`), "starts_at", fp, 2},
		{"unknown field", enc(`{"v":1,"s":"starts_at","k":["a","b"],"f":"` + fp + `","x":1}`), "starts_at", fp, 2},
		{"upper-case key", enc(`{"V":1,"s":"starts_at","k":["a","b"],"f":"` + fp + `"}`), "starts_at", fp, 2},
		{"duplicate key", enc(`{"v":1,"s":"id","s":"starts_at","k":["a","b"],"f":"` + fp + `"}`), "starts_at", fp, 2},
		{"whitespace", enc(`{"v":1, "s":"starts_at","k":["a","b"],"f":"` + fp + `"}`), "starts_at", fp, 2},
		{"trailing data", enc(`{"v":1,"s":"starts_at","k":["a","b"],"f":"` + fp + `"}{}`), "starts_at", fp, 2},
		{"not base64url", valid + "*", "starts_at", fp, 2},
		{"padded base64", base64.URLEncoding.EncodeToString([]byte(`{"v":1,"s":"id","k":["a"]}`)), "id", "", 1},
		{"not JSON", enc("hello"), "starts_at", fp, 2},
		{"too long", strings.Repeat("A", page.MaxCursorLen+1), "starts_at", fp, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := page.Decode(tt.cursor, tt.sort, tt.fp, tt.nvals)
			if c != nil || !errors.Is(err, page.ErrInvalidCursor) {
				t.Fatalf("Decode = %+v, %v; want ErrInvalidCursor", c, err)
			}
		})
	}

	var pe *problem.Error
	if !errors.As(page.ErrInvalidCursor, &pe) || pe.Code() != "page.invalid_cursor" || pe.Status() != http.StatusUnprocessableEntity {
		t.Fatalf("ErrInvalidCursor = %v, want 422 page.invalid_cursor", page.ErrInvalidCursor)
	}
	if page.Fingerprint("ab", "c") == page.Fingerprint("a", "bc") || page.Fingerprint() == page.Fingerprint("") {
		t.Fatal("Fingerprint parts are ambiguous")
	}
}

func TestPage_LimitAndCut(t *testing.T) {
	t.Parallel()
	for in, want := range map[int]int{-1: 25, 0: 25, 1: 1, 25: 25, 100: 100, 101: 100, 1 << 30: 100} {
		if got := page.ClampLimit(in); got != want {
			t.Errorf("ClampLimit(%d) = %d, want %d", in, got, want)
		}
	}
	items, more := page.Cut([]int{1, 2, 3}, 2)
	if len(items) != 2 || !more {
		t.Fatalf("Cut(3 rows, 2) = %v, %v", items, more)
	}
	items, more = page.Cut([]int{1, 2}, 2)
	if len(items) != 2 || more {
		t.Fatalf("Cut(2 rows, 2) = %v, %v", items, more)
	}
}

func FuzzPage_DecodeCursor(f *testing.F) {
	fp := page.Fingerprint("published")
	f.Add(page.Next("starts_at", fp, page.TimeVal(time.Unix(1, 0)), page.UUIDVal(uuid.Nil())))
	f.Add(page.Next("id", "", "x"))
	f.Add("")
	f.Add("e30")
	f.Add(enc(`{"v":1,"s":"starts_at","k":["a","b"],"f":"` + fp + `"}`))
	f.Add(strings.Repeat("A", 600))
	f.Fuzz(func(t *testing.T, s string) {
		c, err := page.Decode(s, "starts_at", fp, 2)
		if err != nil {
			if c != nil || !errors.Is(err, page.ErrInvalidCursor) {
				t.Fatalf("Decode(%q) = %+v, %v", s, c, err)
			}
			return
		}
		if s == "" {
			if c != nil {
				t.Fatalf("empty cursor decoded to %+v", c)
			}
			return
		}
		if c.V != 1 || c.Sort != "starts_at" || c.F != fp || len(c.Vals) != 2 {
			t.Fatalf("Decode(%q) accepted a mismatched cursor %+v", s, c)
		}
		if again := page.Next(c.Sort, c.F, c.Vals...); again != s {
			t.Fatalf("accepted cursor %q is not canonical (%q)", s, again)
		}
		_, _ = c.Time(0)
		_, _ = c.UUID(1)
	})
}
