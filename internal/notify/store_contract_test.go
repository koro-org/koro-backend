package notify_test

import (
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/notify"
)

// runStoreContract is the one body that proves every notify.Store behaves the
// same (05 §13.3). The in-memory store runs it in `make test`; BE-015 runs it
// against the GORM store in `make test-integration`.
func runStoreContract(t *testing.T, newStore func(t *testing.T) notify.Store) {
	t.Helper()
	const lease = 2 * time.Minute

	newDelivery := func(key string) notify.Delivery {
		uid := newID()
		return notify.Delivery{
			ID: newID(), UserID: &uid, RecipientIndex: []byte("bidx"),
			Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
			Template: "notify.test_message", TemplateVersion: 1, DedupeKey: key,
		}
	}
	sentAt := t0.Add(time.Second)
	sent := notify.Result{Status: notify.StatusSent, Provider: "smtp", ProviderMessageID: "m1", SentAt: &sentAt}

	t.Run("a new key is stored as queued with one attempt", func(t *testing.T) {
		t.Parallel()
		s, d := newStore(t), newDelivery("k1")
		got, state, err := s.Claim(t.Context(), d, t0, lease)
		if err != nil || state != notify.ClaimAcquired {
			t.Fatalf("Claim() = (%v, %v)", state, err)
		}
		if got.ID != d.ID || got.Status != notify.StatusQueued || got.Attempts != 1 || !got.CreatedAt.Equal(t0) || !got.UpdatedAt.Equal(t0) ||
			got.DedupeKey != "k1" || got.Template != "notify.test_message" || string(got.RecipientIndex) != "bidx" || got.UserID == nil || *got.UserID != *d.UserID {
			t.Fatalf("stored = %+v, want the queued row of %+v", got, d)
		}
		if read := mustGet(t, s, d.ID); read.DedupeKey != "k1" || read.Status != notify.StatusQueued {
			t.Fatalf("Get() = %+v", read)
		}
	})

	t.Run("a queued delivery within the lease is in flight", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		first, _, _ := s.Claim(t.Context(), newDelivery("k1"), t0, lease)
		got, state, err := s.Claim(t.Context(), newDelivery("k1"), t0.Add(lease-time.Second), lease)
		if err != nil || state != notify.ClaimInFlight || got.ID != first.ID || got.Attempts != 1 {
			t.Fatalf("Claim() = (%+v, %v, %v), want the first delivery in flight", got, state, err)
		}
	})

	t.Run("a queued delivery older than the lease is taken over", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		first, _, _ := s.Claim(t.Context(), newDelivery("k1"), t0, lease)
		later := t0.Add(lease)
		again := newDelivery("k1")
		again.TemplateVersion = 2
		got, state, err := s.Claim(t.Context(), again, later, lease)
		if err != nil || state != notify.ClaimAcquired || got.ID != first.ID || got.Attempts != 2 || got.TemplateVersion != 2 || !got.UpdatedAt.Equal(later) || !got.CreatedAt.Equal(t0) {
			t.Fatalf("Claim() = (%+v, %v, %v), want attempt 2 on the first row with the new template version", got, state, err)
		}
		// The takeover renews the lease.
		if _, state, _ := s.Claim(t.Context(), newDelivery("k1"), later.Add(time.Second), lease); state != notify.ClaimInFlight {
			t.Fatalf("Claim() right after takeover = %v, want in flight", state)
		}
	})

	t.Run("a failed delivery is reopened", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		first, _, _ := s.Claim(t.Context(), newDelivery("k1"), t0, lease)
		if _, err := s.Finish(t.Context(), first.ID, notify.Result{Status: notify.StatusFailed, Provider: "smtp", LastError: "boom"}, t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		got, state, err := s.Claim(t.Context(), newDelivery("k1"), t0.Add(2*time.Second), lease)
		if err != nil || state != notify.ClaimAcquired || got.ID != first.ID || got.Attempts != 2 || got.Status != notify.StatusQueued {
			t.Fatalf("Claim() = (%+v, %v, %v), want the failed row reopened as attempt 2", got, state, err)
		}
	})

	t.Run("a finished delivery is final", func(t *testing.T) {
		t.Parallel()
		for _, status := range []notify.Status{notify.StatusSent, notify.StatusSkippedNoConsent, notify.StatusSuppressed, notify.StatusSkippedPreference} {
			s := newStore(t)
			first, _, _ := s.Claim(t.Context(), newDelivery("k1"), t0, lease)
			if _, err := s.Finish(t.Context(), first.ID, notify.Result{Status: status}, t0.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			got, state, err := s.Claim(t.Context(), newDelivery("k1"), t0.Add(24*time.Hour), lease)
			if err != nil || state != notify.ClaimDone || got.ID != first.ID || got.Status != status || got.Attempts != 1 {
				t.Fatalf("status %s: Claim() = (%+v, %v, %v), want it final", status, got, state, err)
			}
		}
	})

	t.Run("finish overwrites every outcome field, zero values included", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		first, _, _ := s.Claim(t.Context(), newDelivery("k1"), t0, lease)
		if _, err := s.Finish(t.Context(), first.ID, notify.Result{Status: notify.StatusFailed, Provider: "smtp", ProviderMessageID: "m0", LastError: "boom"}, t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Claim(t.Context(), newDelivery("k1"), t0.Add(2*time.Second), lease); err != nil {
			t.Fatal(err)
		}
		done, err := s.Finish(t.Context(), first.ID, sent, t0.Add(3*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if done.Status != notify.StatusSent || done.Provider != "smtp" || done.ProviderMessageID != "m1" || done.LastError != "" ||
			done.SentAt == nil || !done.SentAt.Equal(sentAt) || !done.UpdatedAt.Equal(t0.Add(3*time.Second)) || done.Attempts != 2 {
			t.Fatalf("Finish() = %+v, want the sent outcome with the error cleared and attempts kept", done)
		}
		if read := mustGet(t, s, first.ID); read.LastError != "" || read.Status != notify.StatusSent {
			t.Fatalf("Get() = %+v", read)
		}
	})

	t.Run("finish does not overwrite a final delivery", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		first, _, _ := s.Claim(t.Context(), newDelivery("k1"), t0, lease)
		if _, err := s.Finish(t.Context(), first.ID, sent, t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		late, err := s.Finish(t.Context(), first.ID, notify.Result{Status: notify.StatusFailed, LastError: "late"}, t0.Add(time.Minute))
		if err != nil || late.Status != notify.StatusSent || late.LastError != "" {
			t.Fatalf("late Finish() = (%+v, %v), want the sent delivery untouched", late, err)
		}
		if read := mustGet(t, s, first.ID); read.Status != notify.StatusSent {
			t.Fatalf("Get() = %+v, a late attempt overwrote the outcome", read)
		}
	})

	t.Run("unknown ids are not found", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if _, err := s.Get(t.Context(), newID()); !errors.Is(err, notify.ErrNotFound) {
			t.Fatalf("Get() error = %v, want ErrNotFound", err)
		}
		if _, err := s.Finish(t.Context(), newID(), sent, t0); !errors.Is(err, notify.ErrNotFound) {
			t.Fatalf("Finish() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("different keys are independent", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		a, stateA, _ := s.Claim(t.Context(), newDelivery("a"), t0, lease)
		b, stateB, _ := s.Claim(t.Context(), newDelivery("b"), t0, lease)
		if stateA != notify.ClaimAcquired || stateB != notify.ClaimAcquired || a.ID == b.ID {
			t.Fatalf("claims = (%v %s) (%v %s)", stateA, a.ID, stateB, b.ID)
		}
	})

	t.Run("returned rows are copies", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		d := newDelivery("k1")
		got, _, _ := s.Claim(t.Context(), d, t0, lease)
		got.RecipientIndex[0] = 'X'
		*got.UserID = uuid.Nil()
		d.RecipientIndex[1] = 'Y'
		read := mustGet(t, s, d.ID)
		if string(read.RecipientIndex) != "bidx" || *read.UserID == uuid.Nil() {
			t.Fatalf("stored row changed through a returned copy: %+v", read)
		}
	})

	t.Run("exactly one of many concurrent claims acquires", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		const n = 32
		var wg sync.WaitGroup
		states := make([]notify.ClaimState, n)
		ids := make([]uuid.UUID, n)
		for i := range n {
			wg.Go(func() {
				got, state, err := s.Claim(t.Context(), newDelivery("race"), t0, lease)
				if err != nil {
					t.Errorf("Claim() error = %v", err)
				}
				states[i], ids[i] = state, got.ID
			})
		}
		wg.Wait()
		acquired := 0
		for i, st := range states {
			if st == notify.ClaimAcquired {
				acquired++
			} else if st != notify.ClaimInFlight {
				t.Errorf("claim %d state = %v, want acquired or in flight", i, st)
			}
			if ids[i] != ids[0] {
				t.Errorf("claim %d saw delivery %s, want %s", i, ids[i], ids[0])
			}
		}
		if acquired != 1 {
			t.Fatalf("%d claims acquired the delivery, want exactly 1", acquired)
		}
	})
}

func TestMemStore(t *testing.T) {
	t.Parallel()
	runStoreContract(t, func(*testing.T) notify.Store { return notify.NewMemStore() })
}

func TestMissingStore_FailsEverything(t *testing.T) {
	t.Parallel()
	s := notify.MissingStore{}
	if _, _, err := s.Claim(t.Context(), notify.Delivery{}, t0, time.Minute); !errors.Is(err, notify.ErrStoreNotWired) {
		t.Errorf("Claim() error = %v", err)
	}
	if _, err := s.Finish(t.Context(), newID(), notify.Result{}, t0); !errors.Is(err, notify.ErrStoreNotWired) {
		t.Errorf("Finish() error = %v", err)
	}
	if _, err := s.Get(t.Context(), newID()); !errors.Is(err, notify.ErrStoreNotWired) {
		t.Errorf("Get() error = %v", err)
	}
}
