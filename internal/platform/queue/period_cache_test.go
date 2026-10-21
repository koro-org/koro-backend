package queue_test

import (
	"sync"
	"testing"

	"koro/internal/platform/queue"
)

// TestShortestPeriod_CacheMatchesUncached checks that the memoized result
// equals the uncached computation, on the first (miss) and later (hit)
// calls, including errors, and under concurrent use (-race).
func TestShortestPeriod_CacheMatchesUncached(t *testing.T) {
	t.Parallel()
	specs := []string{
		"@every 10s",
		"@every 1m",
		"*/5 * * * *",
		"10 4 * * *",
		"0 3 * * 1",
		"0 2 1 * *",
		"@daily",
		"not a cron",
		"61 * * * *",
	}
	for _, spec := range specs {
		want, wantErr := queue.ComputeShortestPeriod(spec)
		for i := range 3 {
			got, err := queue.ShortestPeriod(spec)
			if got != want || (err == nil) != (wantErr == nil) || (err != nil && err.Error() != wantErr.Error()) {
				t.Fatalf("%q call %d: got (%v, %v), want (%v, %v)", spec, i, got, err, want, wantErr)
			}
		}
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for _, spec := range specs {
				want, wantErr := queue.ComputeShortestPeriod(spec)
				got, err := queue.ShortestPeriod(spec)
				if got != want || (err == nil) != (wantErr == nil) {
					t.Errorf("%q concurrent: got (%v, %v), want (%v, %v)", spec, got, err, want, wantErr)
				}
			}
		})
	}
	wg.Wait()
}

// benchSpecs mirrors the cron expressions registered in code (one container
// build validates each registered job once).
var benchSpecs = []string{
	"10 4 * * *", "10 4 * * *", "10 4 * * *", "10 4 * * *", "@every 10s", "@every 10s",
	"@every 3s", "@every 2s", "40 4 * * *", "40 * * * *", "35 3 * * *", "35 * * * *",
	"30 9 * * 1-5", "30 4 1 * *", "30 3 * * *", "30 0 * * *", "20 * * * *", "15 2 * * *",
	"15 0 * * *", "15 * * * *", "0 9 * * *", "0 6 * * *", "0 5 * * *", "0 3 * * *",
	"0 2 * * *", "0 10 * * *", "0 1 * * *", "*/15 * * * *", "*/10 * * * *",
}

func BenchmarkShortestPeriod(b *testing.B) {
	b.Run("uncached", func(b *testing.B) {
		for b.Loop() {
			for _, s := range benchSpecs {
				_, _ = queue.ComputeShortestPeriod(s)
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		for b.Loop() {
			for _, s := range benchSpecs {
				_, _ = queue.ShortestPeriod(s)
			}
		}
	})
}
