package identity_test

import (
	"testing"
	"time"
	"uuid"
)

// TestAccountAge_CreationInstants (BE-085): AccountAge returns the
// creation instant of every existing account (the clock of its
// registration) and leaves an unknown ID out; no ID, no read.
func TestAccountAge_CreationInstants(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.activeUser(t, "ilk@example.test")
	e.clk.Advance(72 * time.Hour)
	second := e.activeUser(t, "ikinci@example.test")
	unknown := uuid.NewV7()

	got, err := e.svc.AccountAge(t.Context(), []uuid.UUID{first.ID, second.ID, unknown})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[first.ID].Equal(first.CreatedAt) || !got[second.ID].Equal(second.CreatedAt) {
		t.Fatalf("AccountAge = %v", got)
	}
	if d := got[second.ID].Sub(got[first.ID]); d != 72*time.Hour {
		t.Fatalf("the accounts are %s apart, want 72h", d)
	}
	if got, err := e.svc.AccountAge(t.Context(), nil); err != nil || len(got) != 0 {
		t.Fatalf("AccountAge(nil) = %v, %v", got, err)
	}
}
