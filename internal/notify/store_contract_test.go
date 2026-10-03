package notify_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/notify"
)

// storeWorld is a fresh store plus a way to create users (the GORM store
// needs users rows for its foreign keys; identity owns that table).
type storeWorld struct {
	s    notify.Store
	user func(t *testing.T) uuid.UUID
}

// runStoreContract is the one body that proves every notify.Store behaves the
// same (05 §13.3, T-98). The in-memory store runs it in `make test`; the GORM
// store runs the very same body in `make test-integration`
// (store_integration_test.go).
//
// UpdatedAt is the store's own clock (the database's now() for GORM, see
// DeliveryStore.Claim), so the contract checks that it is set, not its
// value, and tests the lease with lease 0 (always expired) and a long lease.
func runStoreContract(t *testing.T, newWorld func(t *testing.T) storeWorld) {
	t.Helper()
	const lease = 2 * time.Minute

	newDelivery := func(uid uuid.UUID, key string) notify.Delivery {
		return notify.Delivery{
			ID: newID(), UserID: &uid, RecipientIndex: index("a@koro.test"),
			Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
			Template: "notify.test_message", TemplateVersion: 1, DedupeKey: key,
		}
	}
	sentAt := t0.Add(time.Second)
	sent := notify.Result{Status: notify.StatusSent, Provider: "smtp", ProviderMessageID: "m1", SentAt: &sentAt}

	t.Run("a new key is stored as queued with one attempt", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		d := newDelivery(w.user(t), "k1")
		got, state, err := w.s.Claim(t.Context(), d, t0, lease)
		if err != nil || state != notify.ClaimAcquired {
			t.Fatalf("Claim() = (%v, %v)", state, err)
		}
		if got.ID != d.ID || got.Status != notify.StatusQueued || got.Attempts != 1 || !got.CreatedAt.Equal(t0) || got.UpdatedAt.IsZero() ||
			got.DedupeKey != "k1" || got.Template != "notify.test_message" || !bytes.Equal(got.RecipientIndex, d.RecipientIndex) ||
			got.UserID == nil || *got.UserID != *d.UserID {
			t.Fatalf("stored = %+v, want the queued row of %+v", got, d)
		}
		if read := mustGet(t, w.s, d.ID); read.DedupeKey != "k1" || read.Status != notify.StatusQueued {
			t.Fatalf("Get() = %+v", read)
		}
	})

	t.Run("a queued delivery within the lease is in flight", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid := w.user(t)
		first, _, _ := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0, lease)
		got, state, err := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0, time.Hour)
		if err != nil || state != notify.ClaimInFlight || got.ID != first.ID || got.Attempts != 1 {
			t.Fatalf("Claim() = (%+v, %v, %v), want the first delivery in flight", got, state, err)
		}
	})

	t.Run("a queued delivery older than the lease is taken over", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid := w.user(t)
		first, _, _ := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0, lease)
		again := newDelivery(uid, "k1")
		again.TemplateVersion = 2
		got, state, err := w.s.Claim(t.Context(), again, t0, 0)
		if err != nil || state != notify.ClaimAcquired || got.ID != first.ID || got.Attempts != 2 || got.TemplateVersion != 2 ||
			got.UpdatedAt.Before(first.UpdatedAt) || !got.CreatedAt.Equal(t0) {
			t.Fatalf("Claim() = (%+v, %v, %v), want attempt 2 on the first row with the new template version", got, state, err)
		}
		// The takeover renews the lease.
		if _, state, _ := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0, time.Hour); state != notify.ClaimInFlight {
			t.Fatalf("Claim() right after takeover = %v, want in flight", state)
		}
	})

	t.Run("a failed delivery is reopened", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid := w.user(t)
		first, _, _ := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0, lease)
		if _, err := w.s.Finish(t.Context(), first.ID, notify.Result{Status: notify.StatusFailed, Provider: "smtp", LastError: "boom"}, t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		got, state, err := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0.Add(2*time.Second), lease)
		if err != nil || state != notify.ClaimAcquired || got.ID != first.ID || got.Attempts != 2 || got.Status != notify.StatusQueued {
			t.Fatalf("Claim() = (%+v, %v, %v), want the failed row reopened as attempt 2", got, state, err)
		}
	})

	t.Run("a finished delivery is final", func(t *testing.T) {
		t.Parallel()
		for _, status := range []notify.Status{notify.StatusSent, notify.StatusSkippedNoConsent, notify.StatusSuppressed, notify.StatusSkippedPreference} {
			w := newWorld(t)
			uid := w.user(t)
			first, _, _ := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0, lease)
			if _, err := w.s.Finish(t.Context(), first.ID, notify.Result{Status: status}, t0.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			got, state, err := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0.Add(24*time.Hour), 0)
			if err != nil || state != notify.ClaimDone || got.ID != first.ID || got.Status != status || got.Attempts != 1 {
				t.Fatalf("status %s: Claim() = (%+v, %v, %v), want it final", status, got, state, err)
			}
		}
	})

	t.Run("finish overwrites every outcome field, zero values included", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid := w.user(t)
		first, _, _ := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0, lease)
		if _, err := w.s.Finish(t.Context(), first.ID, notify.Result{Status: notify.StatusFailed, Provider: "smtp", ProviderMessageID: "m0", LastError: "boom"}, t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0.Add(2*time.Second), lease); err != nil {
			t.Fatal(err)
		}
		done, err := w.s.Finish(t.Context(), first.ID, sent, t0.Add(3*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if done.Status != notify.StatusSent || done.Provider != "smtp" || done.ProviderMessageID != "m1" || done.LastError != "" ||
			done.SentAt == nil || !done.SentAt.Equal(sentAt) || done.UpdatedAt.IsZero() || done.Attempts != 2 {
			t.Fatalf("Finish() = %+v, want the sent outcome with the error cleared and attempts kept", done)
		}
		if read := mustGet(t, w.s, first.ID); read.LastError != "" || read.Status != notify.StatusSent {
			t.Fatalf("Get() = %+v", read)
		}
	})

	t.Run("finish does not overwrite a final delivery", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		first, _, _ := w.s.Claim(t.Context(), newDelivery(w.user(t), "k1"), t0, lease)
		if _, err := w.s.Finish(t.Context(), first.ID, sent, t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		late, err := w.s.Finish(t.Context(), first.ID, notify.Result{Status: notify.StatusFailed, LastError: "late"}, t0.Add(time.Minute))
		if err != nil || late.Status != notify.StatusSent || late.LastError != "" {
			t.Fatalf("late Finish() = (%+v, %v), want the sent delivery untouched", late, err)
		}
		if read := mustGet(t, w.s, first.ID); read.Status != notify.StatusSent {
			t.Fatalf("Get() = %+v, a late attempt overwrote the outcome", read)
		}
	})

	t.Run("unknown ids are not found", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		if _, err := w.s.Get(t.Context(), newID()); !errors.Is(err, notify.ErrNotFound) {
			t.Fatalf("Get() error = %v, want ErrNotFound", err)
		}
		if _, err := w.s.Finish(t.Context(), newID(), sent, t0); !errors.Is(err, notify.ErrNotFound) {
			t.Fatalf("Finish() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("different keys are independent", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid := w.user(t)
		a, stateA, _ := w.s.Claim(t.Context(), newDelivery(uid, "a"), t0, lease)
		b, stateB, _ := w.s.Claim(t.Context(), newDelivery(uid, "b"), t0, lease)
		if stateA != notify.ClaimAcquired || stateB != notify.ClaimAcquired || a.ID == b.ID {
			t.Fatalf("claims = (%v %s) (%v %s)", stateA, a.ID, stateB, b.ID)
		}
	})

	t.Run("a recipient without an account is stored by its index", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		d := newDelivery(uuid.NewV7(), "invite:1")
		d.UserID = nil
		got, state, err := w.s.Claim(t.Context(), d, t0, lease)
		if err != nil || state != notify.ClaimAcquired || got.UserID != nil || !bytes.Equal(got.RecipientIndex, d.RecipientIndex) {
			t.Fatalf("Claim() = (%+v, %v, %v), want a row with the index and no user", got, state, err)
		}
	})

	t.Run("returned rows are copies", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		d := newDelivery(w.user(t), "k1")
		got, _, _ := w.s.Claim(t.Context(), d, t0, lease)
		want := bytes.Clone(d.RecipientIndex)
		got.RecipientIndex[0] ^= 0xff
		*got.UserID = uuid.Nil()
		d.RecipientIndex[1] ^= 0xff
		read := mustGet(t, w.s, d.ID)
		if !bytes.Equal(read.RecipientIndex, want) || *read.UserID == uuid.Nil() {
			t.Fatalf("stored row changed through a returned copy: %+v", read)
		}
	})

	t.Run("exactly one of many concurrent claims acquires", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid := w.user(t)
		const n = 32
		var wg sync.WaitGroup
		states := make([]notify.ClaimState, n)
		ids := make([]uuid.UUID, n)
		for i := range n {
			wg.Go(func() {
				got, state, err := w.s.Claim(t.Context(), newDelivery(uid, "race"), t0, lease)
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

	t.Run("deliveries of a user page newest first and are erased", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid, other := w.user(t), w.user(t)
		var ids []uuid.UUID
		for i := range 3 {
			d, _, err := w.s.Claim(t.Context(), newDelivery(uid, "u"+string(rune('a'+i))), t0.Add(time.Duration(i)*time.Second), lease)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, d.ID)
		}
		if _, _, err := w.s.Claim(t.Context(), newDelivery(other, "o"), t0, lease); err != nil {
			t.Fatal(err)
		}
		p1, err := w.s.ListOfUser(t.Context(), uid, nil, 2)
		if err != nil || len(p1) != 2 || p1[0].ID != ids[2] || p1[1].ID != ids[1] {
			t.Fatalf("page 1 = %+v (%v), want the two newest", p1, err)
		}
		p2, _ := w.s.ListOfUser(t.Context(), uid, &notify.Position{At: p1[1].CreatedAt, ID: p1[1].ID}, 2)
		if len(p2) != 1 || p2[0].ID != ids[0] {
			t.Fatalf("page 2 = %+v, want the oldest", p2)
		}
		if n, err := w.s.DeleteDeliveriesOf(t.Context(), uid); err != nil || n != 3 {
			t.Fatalf("DeleteDeliveriesOf() = (%d, %v), want 3", n, err)
		}
		if rest, _ := w.s.ListOfUser(t.Context(), other, nil, 10); len(rest) != 1 {
			t.Fatalf("another user's deliveries = %d, want 1 left", len(rest))
		}
		// The key is free again after the erasure.
		if _, state, _ := w.s.Claim(t.Context(), newDelivery(uid, "ua"), t0, lease); state != notify.ClaimAcquired {
			t.Fatalf("Claim() after erasure = %v, want acquired", state)
		}
	})

	t.Run("provider outcomes follow the webhook transitions", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid := w.user(t)
		d, _, _ := w.s.Claim(t.Context(), newDelivery(uid, "k1"), t0, lease)
		ok := notify.Result{Status: notify.StatusSent, Provider: "smtp", ProviderMessageID: d.ID.String() + "@koro.test", SentAt: &sentAt}
		if _, err := w.s.Finish(t.Context(), d.ID, ok, t0); err != nil {
			t.Fatal(err)
		}
		at := t0.Add(time.Minute)
		if found, changed, err := w.s.MarkProviderOutcome(t.Context(), "nobody@koro.test", notify.StatusBounced, at); err != nil || found || changed {
			t.Fatalf("unknown message = (%v, %v, %v)", found, changed, err)
		}
		if found, changed, err := w.s.MarkProviderOutcome(t.Context(), ok.ProviderMessageID, notify.StatusDelivered, at); err != nil || !found || !changed {
			t.Fatalf("delivered = (%v, %v, %v)", found, changed, err)
		}
		if got := mustGet(t, w.s, d.ID); got.Status != notify.StatusDelivered || got.DeliveredAt == nil || !got.DeliveredAt.Equal(at) {
			t.Fatalf("after delivered = %+v", got)
		}
		if _, changed, _ := w.s.MarkProviderOutcome(t.Context(), ok.ProviderMessageID, notify.StatusDelivered, at); changed {
			t.Fatal("delivered twice changed the row")
		}
		if _, changed, _ := w.s.MarkProviderOutcome(t.Context(), ok.ProviderMessageID, notify.StatusComplained, at); !changed {
			t.Fatal("complaint after delivery was not recorded")
		}
		if got := mustGet(t, w.s, d.ID); got.Status != notify.StatusComplained {
			t.Fatalf("status = %s, want complained", got.Status)
		}
		// A skipped delivery never becomes bounced.
		s2, _, _ := w.s.Claim(t.Context(), newDelivery(uid, "k2"), t0, lease)
		if _, err := w.s.Finish(t.Context(), s2.ID, notify.Result{Status: notify.StatusSuppressed, ProviderMessageID: "x@koro.test"}, t0); err != nil {
			t.Fatal(err)
		}
		if found, changed, _ := w.s.MarkProviderOutcome(t.Context(), "x@koro.test", notify.StatusBounced, at); !found || changed {
			t.Fatalf("suppressed delivery = (%v, %v), want found and unchanged", found, changed)
		}
	})

	t.Run("suppressions", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		a, b := index("a@koro.test"), index("b@koro.test")
		if added, err := w.s.Suppress(t.Context(), notify.Suppression{EmailIndex: a, Reason: notify.SuppressHardBounce, Source: notify.SourceProviderWebhook, CreatedAt: t0}); err != nil || !added {
			t.Fatalf("Suppress(a) = (%v, %v)", added, err)
		}
		if added, err := w.s.Suppress(t.Context(), notify.Suppression{EmailIndex: a, Reason: notify.SuppressComplaint, Source: notify.SourceProviderWebhook, CreatedAt: t0}); err != nil || added {
			t.Fatalf("Suppress(a) again = (%v, %v), want no new row", added, err)
		}
		if _, err := w.s.Suppress(t.Context(), notify.Suppression{EmailIndex: b, Reason: notify.SuppressManual, Source: notify.SourceAdmin, CreatedAt: t0.Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
		if yes, _ := w.s.IsSuppressed(t.Context(), a); !yes {
			t.Fatal("a is not suppressed")
		}
		if yes, _ := w.s.IsSuppressed(t.Context(), index("c@koro.test")); yes {
			t.Fatal("c is suppressed")
		}
		got, err := w.s.GetSuppression(t.Context(), a)
		if err != nil || got.Reason != notify.SuppressHardBounce || !got.CreatedAt.Equal(t0) {
			t.Fatalf("GetSuppression(a) = (%+v, %v), want the first reason", got, err)
		}
		p1, _ := w.s.ListSuppressions(t.Context(), nil, 1)
		if len(p1) != 1 || !bytes.Equal(p1[0].EmailIndex, b) {
			t.Fatalf("page 1 = %+v, want b (newest)", p1)
		}
		p2, _ := w.s.ListSuppressions(t.Context(), &notify.SuppressionPosition{At: p1[0].CreatedAt, Index: p1[0].EmailIndex}, 5)
		if len(p2) != 1 || !bytes.Equal(p2[0].EmailIndex, a) {
			t.Fatalf("page 2 = %+v, want a", p2)
		}
		if err := w.s.DeleteSuppression(t.Context(), a); err != nil {
			t.Fatal(err)
		}
		if err := w.s.DeleteSuppression(t.Context(), a); !errors.Is(err, notify.ErrSuppressionNotFound) {
			t.Fatalf("second delete = %v, want ErrSuppressionNotFound", err)
		}
		if _, err := w.s.GetSuppression(t.Context(), a); !errors.Is(err, notify.ErrSuppressionNotFound) {
			t.Fatalf("Get after delete = %v", err)
		}
	})

	t.Run("preferences", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid, other := w.user(t), w.user(t)
		if _, ok, err := w.s.Preference(t.Context(), uid, notify.PrefEventUpdates, notify.ChannelEmail); err != nil || ok {
			t.Fatalf("Preference() before any row = (%v, %v), want none", ok, err)
		}
		err := w.s.UpsertPreferences(t.Context(), uid, []notify.Preference{
			{Category: notify.PrefEventUpdates, Channel: notify.ChannelEmail, Enabled: false},
			{Category: notify.PrefSecurity, Channel: notify.ChannelPush, Enabled: true},
		}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.s.UpsertPreferences(t.Context(), uid, []notify.Preference{{Category: notify.PrefEventUpdates, Channel: notify.ChannelEmail, Enabled: true}}, t0); err != nil {
			t.Fatal(err)
		}
		if on, ok, _ := w.s.Preference(t.Context(), uid, notify.PrefEventUpdates, notify.ChannelEmail); !ok || !on {
			t.Fatal("the update did not overwrite the row")
		}
		if err := w.s.UpsertPreferences(t.Context(), uid, []notify.Preference{{Category: notify.PrefSecurity, Channel: notify.ChannelEmail, Enabled: false}}, t0); !errors.Is(err, notify.ErrSecurityLocked) {
			t.Fatalf("disabling security = %v, want ErrSecurityLocked", err)
		}
		_ = w.s.UpsertPreferences(t.Context(), other, []notify.Preference{{Category: notify.PrefModeration, Channel: notify.ChannelPush, Enabled: false}}, t0)
		rows, _ := w.s.Preferences(t.Context(), uid)
		if len(rows) != 2 {
			t.Fatalf("Preferences() = %+v, want the user's two rows", rows)
		}
		if n, err := w.s.DeletePreferencesOf(t.Context(), uid); err != nil || n != 2 {
			t.Fatalf("DeletePreferencesOf() = (%d, %v)", n, err)
		}
		if rows, _ := w.s.Preferences(t.Context(), other); len(rows) != 1 {
			t.Fatal("another user's preference was deleted")
		}
	})

	t.Run("in-app notifications", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid, other := w.user(t), w.user(t)
		mk := func(user uuid.UUID, i int) notify.InAppNotification {
			return notify.InAppNotification{ID: newID(), UserID: user, Kind: "notify.test_notice", Title: "Başlık", Body: "Gövde",
				LinkPath: "/me/notifications", Data: map[string]string{"event_id": newID().String()}, CreatedAt: t0.Add(time.Duration(i) * time.Second)}
		}
		n1, n2, n3 := mk(uid, 0), mk(uid, 1), mk(other, 2)
		for _, n := range []notify.InAppNotification{n1, n2, n3} {
			if ins, err := w.s.InsertInApp(t.Context(), n); err != nil || !ins {
				t.Fatalf("InsertInApp() = (%v, %v)", ins, err)
			}
		}
		if ins, err := w.s.InsertInApp(t.Context(), n1); err != nil || ins {
			t.Fatalf("InsertInApp(same id) = (%v, %v), want no new row", ins, err)
		}
		all, _ := w.s.ListInApp(t.Context(), uid, false, nil, 10)
		if len(all) != 2 || all[0].ID != n2.ID || all[1].Data["event_id"] != n1.Data["event_id"] || all[1].LinkPath != "/me/notifications" {
			t.Fatalf("ListInApp() = %+v, want n2, n1", all)
		}
		page2, _ := w.s.ListInApp(t.Context(), uid, false, &notify.Position{At: all[0].CreatedAt, ID: all[0].ID}, 10)
		if len(page2) != 1 || page2[0].ID != n1.ID {
			t.Fatalf("page 2 = %+v", page2)
		}
		if err := w.s.MarkInAppRead(t.Context(), uid, n3.ID, t0); !errors.Is(err, notify.ErrNotificationNotFound) {
			t.Fatalf("marking another user's notification = %v, want not found", err)
		}
		if err := w.s.MarkInAppRead(t.Context(), uid, n1.ID, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := w.s.MarkInAppRead(t.Context(), uid, n1.ID, t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		unread, _ := w.s.ListInApp(t.Context(), uid, true, nil, 10)
		if len(unread) != 1 || unread[0].ID != n2.ID {
			t.Fatalf("unread = %+v, want n2", unread)
		}
		read, _ := w.s.ListInApp(t.Context(), uid, false, nil, 10)
		if read[1].ReadAt == nil || !read[1].ReadAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("read_at = %v, want the first mark", read[1].ReadAt)
		}
		if n, err := w.s.MarkAllInAppRead(t.Context(), uid, t0); err != nil || n != 1 {
			t.Fatalf("MarkAllInAppRead() = (%d, %v), want 1", n, err)
		}
		if rest, _ := w.s.ListInApp(t.Context(), other, true, nil, 10); len(rest) != 1 {
			t.Fatal("another user's notification was marked read")
		}
		if n, err := w.s.DeleteInAppOf(t.Context(), uid); err != nil || n != 2 {
			t.Fatalf("DeleteInAppOf() = (%d, %v)", n, err)
		}
	})

	t.Run("push tokens", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		uid, other := w.user(t), w.user(t)
		tok := func(user uuid.UUID, raw string) notify.PushToken {
			sum := sha256.Sum256([]byte(raw))
			return notify.PushToken{ID: newID(), UserID: user, Platform: notify.PushIOS, Token: raw, TokenSHA256: sum[:],
				AppVersion: "1.0", CreatedAt: t0, LastSeenAt: t0}
		}
		first, err := w.s.UpsertPushToken(t.Context(), tok(uid, "device-a"))
		if err != nil || first.UserID != uid {
			t.Fatalf("UpsertPushToken() = (%+v, %v)", first, err)
		}
		moved := tok(other, "device-a")
		moved.Platform, moved.LastSeenAt = notify.PushAndroid, t0.Add(time.Hour)
		got, err := w.s.UpsertPushToken(t.Context(), moved)
		if err != nil || got.ID != first.ID || got.UserID != other || got.Platform != notify.PushAndroid || !got.LastSeenAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("re-register = (%+v, %v), want the same row moved to the other user", got, err)
		}
		if mine, _ := w.s.ActivePushTokens(t.Context(), uid); len(mine) != 0 {
			t.Fatalf("the first user still has %d tokens", len(mine))
		}
		second, _ := w.s.UpsertPushToken(t.Context(), tok(uid, "device-b"))
		if err := w.s.DeletePushToken(t.Context(), uid, got.ID); !errors.Is(err, notify.ErrPushTokenNotFound) {
			t.Fatalf("deleting another user's token = %v, want not found", err)
		}
		if all, _ := w.s.PushTokensOf(t.Context(), uid); len(all) != 1 || all[0].ID != second.ID || all[0].Token != "device-b" {
			t.Fatalf("PushTokensOf() = %+v", all)
		}
		if err := w.s.DeletePushToken(t.Context(), uid, second.ID); err != nil {
			t.Fatal(err)
		}
		if n, err := w.s.DeletePushTokensOf(t.Context(), other); err != nil || n != 1 {
			t.Fatalf("DeletePushTokensOf() = (%d, %v)", n, err)
		}
	})
}

// index is a 32-byte blind index for the contract (the table CHECK).
func index(email string) []byte { return testIndexer{}.Index(email) }

func TestMemStore(t *testing.T) {
	t.Parallel()
	runStoreContract(t, func(*testing.T) storeWorld {
		return storeWorld{s: notify.NewMemStore(), user: func(*testing.T) uuid.UUID { return newID() }}
	})
}
