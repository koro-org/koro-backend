package parties_test

import (
	"testing"

	"koro/internal/parties"
)

func TestValidVKN(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"1234567890", true}, {"9876543217", true}, {"1111111114", true}, {"1000000000", true},
		{"1234567891", false}, // wrong check digit
		{"9876543210", false},
		{"123456789", false}, {"12345678901", false}, {"123456789a", false}, {"", false}, {"12345 7890", false},
	} {
		if got := parties.ValidVKN(tc.in); got != tc.want {
			t.Errorf("ValidVKN(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestValidTCKN(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"10000000146", true}, {"12345678950", true},
		{"10000000147", false}, // second check digit
		{"10000000156", false}, // first check digit
		{"01234567890", false}, // leading zero
		{"1000000014", false}, {"100000001466", false}, {"1000000014a", false}, {"", false},
	} {
		if got := parties.ValidTCKN(tc.in); got != tc.want {
			t.Errorf("ValidTCKN(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestValidMERSIS(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"0123456789012345", true}, {"9999999999999999", true},
		{"012345678901234", false}, {"01234567890123456", false}, {"012345678901234x", false}, {"", false},
	} {
		if got := parties.ValidMERSIS(tc.in); got != tc.want {
			t.Errorf("ValidMERSIS(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
