package fake_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"koro/internal/platform/testutil/fake"
)

func TestFake_DeterministicPerSeed(t *testing.T) {
	t.Parallel()
	a, b, c := fake.New(42), fake.New(42), fake.New(43)
	var sameA, sameB, other []string
	for range 20 {
		sameA = append(sameA, a.FullName(), a.University(), a.Department(), a.Club(), a.Email(), a.Phone())
		sameB = append(sameB, b.FullName(), b.University(), b.Department(), b.Club(), b.Email(), b.Phone())
		other = append(other, c.FullName())
	}
	if strings.Join(sameA, "|") != strings.Join(sameB, "|") {
		t.Fatal("same seed produced different data")
	}
	if strings.Join(sameA[:20], "|") == strings.Join(other, "|") {
		t.Fatal("different seeds produced the same names")
	}
}

var (
	emailRe = regexp.MustCompile(`^[a-z]+\.[a-z]+\.\d+@example\.test$`)
	phoneRe = regexp.MustCompile(`^\+90 000 000 00 \d{2}$`)
)

func TestFake_SyntheticContactData(t *testing.T) {
	t.Parallel()
	f := fake.New(7)
	seen := map[string]bool{}
	for range 500 {
		e := f.Email()
		if !emailRe.MatchString(e) {
			t.Fatalf("email %q is not a synthetic @example.test address", e)
		}
		if seen[e] {
			t.Fatalf("duplicate email %q", e)
		}
		seen[e] = true
		if p := f.Phone(); !phoneRe.MatchString(p) {
			t.Fatalf("phone %q does not follow +90 000 000 00 NN", p)
		}
	}
	if got := fake.ASCIIFold("İlkay IŞIK Çağrı Öğüt"); got != "ilkayisikcagriogut" {
		t.Fatalf("ASCIIFold = %q", got)
	}
}

func tcknValid(s string) bool {
	if len(s) != 11 || s[0] == '0' {
		return false
	}
	d := make([]int, 11)
	for i, c := range s {
		n, err := strconv.Atoi(string(c))
		if err != nil {
			return false
		}
		d[i] = n
	}
	odd := d[0] + d[2] + d[4] + d[6] + d[8]
	even := d[1] + d[3] + d[5] + d[7]
	sum10 := 0
	for _, v := range d[:10] {
		sum10 += v
	}
	return ((odd*7-even)%10+10)%10 == d[9] && sum10%10 == d[10]
}

func TestFake_TCKNVectorsAreSelfConsistent(t *testing.T) {
	t.Parallel()
	valid := 0
	for _, v := range fake.TCKNVectors {
		if got := tcknValid(v.Input); got != v.Valid {
			t.Errorf("TCKN vector %q (%s): reference = %v, documented %v", v.Input, v.Note, got, v.Valid)
		}
		if v.Valid {
			valid++
		}
	}
	if valid == 0 || valid == len(fake.TCKNVectors) {
		t.Fatalf("TCKN vectors need valid and invalid cases (valid=%d)", valid)
	}
}
