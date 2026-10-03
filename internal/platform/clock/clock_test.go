package clock_test

import (
	"testing"
	"time"

	_ "time/tzdata" // the production binary embeds tzdata too (cmd/koro)

	"koro/internal/platform/clock"
)

func TestFake_AdvanceAndSet(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 3, 9, 0, 0, 0, time.FixedZone("x", 3*3600))
	f := clock.NewFake(t0)
	if got := f.Now(); !got.Equal(t0) || got.Location() != time.UTC {
		t.Fatalf("Now() = %v, want %v in UTC", got, t0)
	}
	f.Advance(90 * time.Minute)
	if got := f.Now(); !got.Equal(t0.Add(90 * time.Minute)) {
		t.Fatalf("after Advance Now() = %v", got)
	}
	t1 := t0.Add(-time.Hour)
	f.Set(t1)
	if got := f.Now(); !got.Equal(t1) {
		t.Fatalf("after Set Now() = %v", got)
	}
}

func TestReal_ReturnsUTC(t *testing.T) {
	t.Parallel()
	if loc := clock.Real().Now().Location(); loc != time.UTC {
		t.Fatalf("Real().Now() location = %v, want UTC", loc)
	}
}

func TestIstanbul_FixedOffsetPlus3(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC).In(clock.Istanbul())
	if _, off := at.Zone(); off != 3*3600 {
		t.Fatalf("Europe/Istanbul offset = %d, want +3h", off)
	}
}
