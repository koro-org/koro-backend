package iban_test

import (
	"errors"
	"math/big"
	"regexp"
	"strings"
	"testing"

	"koro/internal/platform/iban"
	"koro/internal/platform/iban/ibantest"
)

var trFormat = regexp.MustCompile(`^TR[0-9]{24}$`)

// refValid is an independent big-integer implementation of the rule in
// 02 §0.3 (koro_iban_tr_valid).
func refValid(s string) bool {
	if !trFormat.MatchString(s) {
		return false
	}
	n, ok := new(big.Int).SetString(s[4:]+"2927"+s[2:4], 10)
	return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

func TestIBAN_ValidAndInvalidVectors(t *testing.T) {
	t.Parallel()
	vectors := ibantest.Vectors()
	var valid, invalid int
	for _, v := range vectors {
		t.Run(v.Note, func(t *testing.T) {
			t.Parallel()
			if got := iban.Valid(v.IBAN); got != v.Valid {
				t.Fatalf("Valid(%q) = %v, want %v", v.IBAN, got, v.Valid)
			}
			if got := refValid(v.IBAN); got != v.Valid {
				t.Fatalf("reference Valid(%q) = %v: vector is wrong", v.IBAN, got)
			}
		})
		if v.Valid {
			valid++
		} else {
			invalid++
		}
	}
	if valid < 5 || invalid < 10 {
		t.Fatalf("too few vectors: %d valid, %d invalid", valid, invalid)
	}

	parse := []struct {
		in      string
		want    string
		wantErr error
	}{
		{"TR330006100519786457841326", "TR330006100519786457841326", nil},
		{"TR33 0006 1005 1978 6457 8413 26", "TR330006100519786457841326", nil},
		{"  tr33 0006 1005 1978 6457 8413 26\n", "TR330006100519786457841326", nil},
		{"Tr33\t0006\u00a01005 1978 6457 8413 26", "TR330006100519786457841326", nil},
		{"TR33-0006-1005-1978-6457-8413-26", "", iban.ErrInvalid},
		{"TR760001200945200058000003", "", iban.ErrInvalid},
		{"DE89 3704 0044 0532 0130 00", "", iban.ErrInvalid},
		{"", "", iban.ErrInvalid},
		{"ＴＲ330006100519786457841326", "", iban.ErrInvalid},
	}
	for _, p := range parse {
		got, err := iban.Parse(p.in)
		if got != p.want || !errors.Is(err, p.wantErr) {
			t.Errorf("Parse(%q) = %q, %v; want %q, %v", p.in, got, err, p.want, p.wantErr)
		}
	}
}

func TestIBAN_Mask(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"TR330006100519786457841326", "TR33 **** **** **** **** 8413 26"},
		{"tr33 0006 1005 1978 6457 8413 26", "TR33 **** **** **** **** 8413 26"},
		{"TR950001200945200058000003", "TR95 **** **** **** **** 0000 03"},
		{"TR3300061005197864578413", "****"},
		{"DE89370400440532013000", "****"},
		{"", "****"},
		{"XX330006100519786457841326", "****"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got := iban.Mask(tt.in)
			if got != tt.want {
				t.Fatalf("Mask(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if digits := strings.Count(got, "*"); got != "****" && digits != 16 {
				t.Fatalf("Mask(%q) hides %d digits, want 16", tt.in, digits)
			}
		})
	}
}

func TestIBAN_Format(t *testing.T) {
	t.Parallel()
	if got := iban.Format("tr330006100519786457841326"); got != "TR33 0006 1005 1978 6457 8413 26" {
		t.Fatalf("Format = %q", got)
	}
	if got := iban.Format(""); got != "" {
		t.Fatalf("Format(\"\") = %q", got)
	}
}

func FuzzIBAN_Parse(f *testing.F) {
	for _, v := range ibantest.Vectors() {
		f.Add(v.IBAN)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := iban.Valid(s), refValid(s); got != want {
			t.Fatalf("Valid(%q) = %v, reference %v", s, got, want)
		}
		n, err := iban.Parse(s)
		if err != nil {
			if !errors.Is(err, iban.ErrInvalid) || n != "" {
				t.Fatalf("Parse(%q) = %q, %v", s, n, err)
			}
			return
		}
		if !iban.Valid(n) || iban.Normalize(n) != n {
			t.Fatalf("Parse(%q) = %q is not a valid normalized IBAN", s, n)
		}
		if m := iban.Mask(s); strings.ReplaceAll(m, " ", "") != n[:4]+strings.Repeat("*", 16)+n[20:] {
			t.Fatalf("Mask(%q) = %q leaks the middle digits", s, m)
		}
	})
}
