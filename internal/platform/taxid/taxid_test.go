package taxid_test

import (
	"testing"

	"koro/internal/platform/taxid"
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
		if got := taxid.ValidVKN(tc.in); got != tc.want {
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
		if got := taxid.ValidTCKN(tc.in); got != tc.want {
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
		if got := taxid.ValidMERSIS(tc.in); got != tc.want {
			t.Errorf("ValidMERSIS(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeAndMask(t *testing.T) {
	t.Parallel()
	if d, ok := taxid.VKN(" 123.456-7890 "); !ok || d != "1234567890" {
		t.Errorf("VKN = %q, %v", d, ok)
	}
	if _, ok := taxid.VKN("1234567891"); ok {
		t.Error("VKN accepted a wrong check digit")
	}
	if d, ok := taxid.TCKN("100 000 001 46"); !ok || d != "10000000146" {
		t.Errorf("TCKN = %q, %v", d, ok)
	}
	if _, ok := taxid.TCKN("1234567890"); ok {
		t.Error("TCKN accepted a VKN")
	}
	if d, ok := taxid.MERSIS("0123-4567-8901-2345"); !ok || d != "0123456789012345" {
		t.Errorf("MERSIS = %q, %v", d, ok)
	}
	if _, ok := taxid.Digits(""); ok {
		t.Error("Digits accepted an empty value")
	}
	if got := taxid.Mask("1234567890", taxid.MaskedDigits); got != "******7890" {
		t.Errorf("Mask = %q", got)
	}
	if got := taxid.Mask("123", 4); got != "***" {
		t.Errorf("Mask of a short value = %q", got)
	}
}
