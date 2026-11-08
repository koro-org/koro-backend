package outreach

import (
	"bytes"
	"strings"
	"testing"
	"uuid"
)

// offsetBy is a Noise that answers off within the range.
type offsetBy int

func (o offsetBy) Offset(_ []byte, half int) (int, error) { return max(-half, min(half, int(o))), nil }

// TestNoiseHalf pins the offset range: half the bucket step of the size.
func TestNoiseHalf(t *testing.T) {
	t.Parallel()
	for n, want := range map[int]int{10: 5, 99: 5, 100: 25, 999: 25, 1_000: 50, 9_999: 50, 10_000: 500, 123_456: 500} {
		if got := noiseHalf(n); got != want {
			t.Fatalf("noiseHalf(%d) = %d, want %d", n, got, want)
		}
	}
}

// TestNoiseSeed_CanonicalAndSizeBound: the seed is the canonical
// definition (order and duplicates do not matter) and the exact size; a
// different size, another definition or registrants instead of attendees
// is another seed.
func TestNoiseSeed_CanonicalAndSizeBound(t *testing.T) {
	t.Parallel()
	a, b, ev := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	norm := func(d Definition) Definition {
		t.Helper()
		n, err := d.normalize("")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	one := norm(Definition{Uni: []uuid.UUID{a, b, a}, Year: []int16{4, 3}})
	two := norm(Definition{Year: []int16{3, 4, 3}, Uni: []uuid.UUID{b, a}})
	if !bytes.Equal(noiseSeed(one, 40), noiseSeed(two, 40)) {
		t.Fatal("the same definition written otherwise has another seed")
	}
	seeds := [][]byte{
		noiseSeed(one, 40),
		noiseSeed(one, 41),
		noiseSeed(norm(Definition{Uni: []uuid.UUID{a}, Year: []int16{3, 4}}), 40),
		noiseSeed(norm(Definition{Event: []uuid.UUID{ev}}), 40),
		noiseSeed(norm(Definition{Event: []uuid.UUID{ev}, EventAttended: true}), 40),
	}
	for i := range seeds {
		for j := i + 1; j < len(seeds); j++ {
			if bytes.Equal(seeds[i], seeds[j]) {
				t.Fatalf("seeds %d and %d are equal: %q", i, j, seeds[i])
			}
		}
	}
}

// TestPublish_AtMostOneBucketAway: with the largest offsets either way, a
// published estimate is never more than one bucket step from the plain
// bucket of the size, and below 10 after the offset there is no number.
func TestPublish_AtMostOneBucketAway(t *testing.T) {
	t.Parallel()
	for _, off := range []offsetBy{-1000, 0, 1000} {
		s := &service{d: Deps{Noise: off}}
		for n := KThreshold; n < 30_000; n++ {
			est, err := s.publish(Definition{Year: []int16{3}}, n)
			if err != nil {
				t.Fatal(err)
			}
			shifted := n + max(-noiseHalf(n), min(noiseHalf(n), int(off)))
			if shifted < KThreshold {
				if est.Count != nil || !est.BelowThreshold {
					t.Fatalf("n %d offset %d: %+v, want below the threshold", n, off, est)
				}
				continue
			}
			if est.Count == nil {
				t.Fatalf("n %d offset %d: no number", n, off)
			}
			if d := *est.Count - Bucket(n); d < -bucketStep(n) || d > bucketStep(n) {
				t.Fatalf("n %d offset %d: %d is %d from the bucket %d", n, off, *est.Count, d, Bucket(n))
			}
		}
	}
}

// TestFamilies: one family per present dimension (in dimensionNames
// order); two definitions that differ only inside one dimension share that
// dimension's family and no other.
func TestFamilies(t *testing.T) {
	t.Parallel()
	d1, d2, ev := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	a := families(Definition{Dept: []uuid.UUID{d1}, Year: []int16{4}})
	b := families(Definition{Dept: []uuid.UUID{d1, d2}, Year: []int16{4}})
	if len(a) != 2 || len(b) != 2 || a[0] != b[0] || a[1] == b[1] {
		t.Fatalf("dept differs: families %v and %v, want the dept family shared only", a, b)
	}
	if !strings.HasPrefix(a[0], "dept:") || !strings.HasPrefix(a[1], "year:") {
		t.Fatalf("family names %v", a)
	}
	// Registrants and attendees of the same events differ only in the event dimension.
	r := families(Definition{Event: []uuid.UUID{ev}, Year: []int16{4}})
	at := families(Definition{Event: []uuid.UUID{ev}, EventAttended: true, Year: []int16{4}})
	if len(r) != 2 || r[1] != at[1] || r[0] == at[0] {
		t.Fatalf("event differs: families %v and %v, want the event family shared only", r, at)
	}
}
