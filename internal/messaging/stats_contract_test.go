package messaging_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/messaging"
)

func TestStatsStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	statsContract(t, func(t *testing.T) (messaging.Store, world) {
		return messaging.NewMemStore(time.Now), memWorld()
	})
}

// statsContract is the request counts of a company's panel (BE-163): the
// requests a company sent in a range by effective status (a pending one
// past its expiry is expired); another company's requests are left out.
func statsContract(t *testing.T, newStore func(t *testing.T) (messaging.Store, world)) {
	t.Helper()
	t.Run("contact request stats", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, sender := w.org(t), w.org(t), w.user(t)
		now := time.Now().UTC().Truncate(time.Microsecond)
		insert := func(o uuid.UUID, expires time.Time) messaging.ContactRequest {
			t.Helper()
			r, err := s.Insert(ctx, messaging.ContactRequest{ID: uuid.NewV7(), OrgID: o, SenderUserID: sender, StudentUserID: w.user(t),
				Via: messaging.ViaDiscovery, Subject: "Yaz stajı", Message: "Profilinizi gördük, görüşmek isteriz.",
				Status: messaging.StatusPending, ExpiresAt: expires, EntitlementUsageID: w.usage(t, o)})
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		later := now.Add(messaging.RequestTTL)
		insert(org, later)
		insert(org, now.Add(-time.Minute)) // expired by now
		for _, to := range []messaging.Status{messaging.StatusAccepted, messaging.StatusAccepted, messaging.StatusDeclined} {
			r := insert(org, later)
			if _, ok, err := s.Transition(ctx, r.ID, messaging.StatusPending, to, &now); err != nil || !ok {
				t.Fatalf("Transition: %v %v", ok, err)
			}
		}
		insert(other, later)
		st, err := s.ContactRequestStats(ctx, org, now.Add(-time.Hour), now.Add(time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		if st.Sent != 5 || st.ByStatus[messaging.StatusPending] != 1 || st.ByStatus[messaging.StatusExpired] != 1 ||
			st.ByStatus[messaging.StatusAccepted] != 2 || st.ByStatus[messaging.StatusDeclined] != 1 {
			t.Fatalf("stats = %+v", st)
		}
		if old, err := s.ContactRequestStats(ctx, org, now.Add(-2*time.Hour), now.Add(-time.Hour), now); err != nil || old.Sent != 0 {
			t.Fatalf("a range before = %+v, %v", old, err)
		}
	})
}
