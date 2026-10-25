package textx_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"koro/internal/platform/textx"
)

func TestTrimSlug(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"}, {"abc-def", 4, "abc"}, {"abc--def", 5, "abc"}, {"abc-def", 7, "abc-def"}, {"abcdef", 3, "abc"},
	} {
		if got := textx.TrimSlug(tc.in, tc.n); got != tc.want {
			t.Errorf("TrimSlug(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestFreeSlug_SequenceThenRandom(t *testing.T) {
	t.Parallel()
	var tried []string
	ok, err := textx.FreeSlug("kulup-adi", 12, 3, func(s string) (bool, error) {
		tried = append(tried, s)
		return false, nil
	})
	if ok || err != nil {
		t.Fatalf("all taken: ok=%v err=%v", ok, err)
	}
	if len(tried) != 4 || tried[0] != "kulup-adi" || tried[1] != "kulup-adi-2" || tried[2] != "kulup-adi-3" {
		t.Fatalf("tried = %q", tried)
	}
	if !regexp.MustCompile(`^kul-[0-9a-f]{8}$`).MatchString(tried[3]) {
		t.Fatalf("random candidate = %q (cut to 12 bytes before the suffix, no trailing hyphen)", tried[3])
	}

	tried = nil
	ok, err = textx.FreeSlug("a", 80, 20, func(s string) (bool, error) {
		tried = append(tried, s)
		return s == "a-2", nil
	})
	if !ok || err != nil || strings.Join(tried, ",") != "a,a-2" {
		t.Fatalf("second free: ok=%v err=%v tried=%q", ok, err, tried)
	}

	boom := errors.New("db down")
	ok, err = textx.FreeSlug("a", 80, 20, func(string) (bool, error) { return false, boom })
	if ok || !errors.Is(err, boom) {
		t.Fatalf("insert error: ok=%v err=%v", ok, err)
	}
}
