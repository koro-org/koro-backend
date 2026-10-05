package entitlements_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/entitlements"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// world creates the parent rows a store needs: orgs and users (FKs in
// PostgreSQL; plain IDs in memory).
type world struct {
	org  func(t *testing.T) uuid.UUID
	user func(t *testing.T) uuid.UUID
}

func memWorld() world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	return world{org: id, user: id}
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (entitlements.Store, world)) {
	t.Helper()
	now := t0
	quota := func(org, admin uuid.UUID, f entitlements.Feature, total int, from, to time.Time) entitlements.Entitlement {
		return entitlements.Entitlement{
			ID: uuid.NewV7(), OrgID: org, Feature: f, Kind: entitlements.KindQuota, QuantityTotal: &total,
			ValidFrom: from, ValidTo: to, Status: entitlements.StatusActive, SourceType: entitlements.SourceAdminGrant,
			GrantedBy: &admin, ActivatedAt: &from, CreatedAt: now, UpdatedAt: now,
		}
	}
	insert := func(t *testing.T, s entitlements.Store, e entitlements.Entitlement) entitlements.Entitlement {
		t.Helper()
		if err := s.Insert(t.Context(), e); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		return e
	}
	day := 24 * time.Hour

	t.Run("insert, get and org scope", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		org, other, admin := w.org(t), w.org(t), w.user(t)
		e := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 5, now.Add(-day), now.Add(day)))
		got, err := s.Get(t.Context(), org, e.ID)
		if err != nil || got.ID != e.ID || *got.QuantityTotal != 5 || got.Status != entitlements.StatusActive ||
			!got.ValidTo.Equal(e.ValidTo) || got.GrantedBy == nil || *got.GrantedBy != admin {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		if _, err := s.Get(t.Context(), other, e.ID); !errors.Is(err, entitlements.ErrNotFound) {
			t.Fatalf("Get under another org: %v", err)
		}
		if _, err := s.Get(t.Context(), uuid.Nil(), e.ID); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("Get without org: %v", err)
		}
		if _, err := s.Lock(t.Context(), uuid.NewV7()); !errors.Is(err, entitlements.ErrNotFound) {
			t.Fatalf("Lock of a missing row: %v", err)
		}
		// CHECK constraints: an access right with a total is 422.
		bad := quota(org, admin, entitlements.FeatureDiscoveryAccess, 5, now, now.Add(day))
		if err := s.Insert(t.Context(), bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("Insert of an invalid row: %v", err)
		}
		// entitlements_source_uq: a source grants a feature once.
		line := uuid.NewV7()
		src := quota(org, admin, entitlements.FeatureContactRequest, 5, now, now.Add(day))
		src.SourceType, src.SourceID, src.GrantedBy = entitlements.SourceOrderLine, &line, nil
		insert(t, s, src)
		dup := src
		dup.ID = uuid.NewV7()
		if err := s.Insert(t.Context(), dup); !errors.Is(err, problem.ErrConflict) {
			t.Fatalf("Insert of a duplicate source: %v", err)
		}
		got2, err := s.BySource(t.Context(), entitlements.SourceOrderLine, line)
		if err != nil || len(got2) != 1 || got2[0].ID != src.ID {
			t.Fatalf("BySource = %+v, %v", got2, err)
		}
	})

	t.Run("list and usable", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		org, other, admin := w.org(t), w.org(t), w.user(t)
		late := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 5, now.Add(-day), now.Add(9*day)))
		early := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 5, now.Add(-day), now.Add(2*day)))
		insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 5, now.Add(day), now.Add(3*day)))  // not started
		insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 5, now.Add(-day), now))            // ended
		insert(t, s, quota(org, admin, entitlements.FeatureContactRequest, 5, now.Add(-day), now.Add(day))) // other feature
		insert(t, s, quota(other, admin, entitlements.FeatureTargetedPost, 5, now.Add(-day), now.Add(day))) // other org
		u, err := s.Usable(t.Context(), org, entitlements.FeatureTargetedPost, now)
		if err != nil || len(u) != 2 || u[0].ID != early.ID || u[1].ID != late.ID {
			t.Fatalf("Usable = %+v, %v", u, err)
		}
		all, err := s.List(t.Context(), org, entitlements.ListFilter{}, nil, 10)
		if err != nil || len(all) != 5 {
			t.Fatalf("List = %d, %v", len(all), err)
		}
		for i := 1; i < len(all); i++ {
			if all[i-1].ID.Compare(all[i].ID) <= 0 {
				t.Fatal("List is not newest first")
			}
		}
		page2, err := s.List(t.Context(), org, entitlements.ListFilter{}, &all[1].ID, 2)
		if err != nil || len(page2) != 2 || page2[0].ID != all[2].ID {
			t.Fatalf("List after = %+v, %v", page2, err)
		}
		f, err := s.List(t.Context(), org, entitlements.ListFilter{Feature: entitlements.FeatureContactRequest, Status: entitlements.StatusActive}, nil, 10)
		if err != nil || len(f) != 1 {
			t.Fatalf("filtered List = %+v, %v", f, err)
		}
		if _, err := s.List(t.Context(), uuid.Nil(), entitlements.ListFilter{}, nil, 10); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("List without org: %v", err)
		}
	})

	t.Run("try consume and unconsume", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		org, other, admin := w.org(t), w.org(t), w.user(t)
		e := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 2, now.Add(-day), now.Add(day)))
		ctx := t.Context()
		if _, ok, err := s.TryConsume(ctx, other, e.ID, 1, now); ok || err != nil {
			t.Fatalf("TryConsume under another org = %v, %v", ok, err)
		}
		if _, ok, err := s.TryConsume(ctx, org, e.ID, 3, now); ok || err != nil {
			t.Fatalf("TryConsume over the total = %v, %v", ok, err)
		}
		if _, ok, err := s.TryConsume(ctx, org, e.ID, 1, now.Add(day)); ok || err != nil {
			t.Fatalf("TryConsume at valid_to = %v, %v", ok, err)
		}
		got, ok, err := s.TryConsume(ctx, org, e.ID, 1, now)
		if !ok || err != nil || got.QuantityUsed != 1 || got.Status != entitlements.StatusActive || got.OrgID != org {
			t.Fatalf("TryConsume = %+v, %v, %v", got, ok, err)
		}
		got, ok, err = s.TryConsume(ctx, org, e.ID, 1, now)
		if !ok || err != nil || got.QuantityUsed != 2 || got.Status != entitlements.StatusExhausted {
			t.Fatalf("TryConsume of the last unit = %+v, %v, %v", got, ok, err)
		}
		if _, ok, err := s.TryConsume(ctx, org, e.ID, 1, now); ok || err != nil {
			t.Fatalf("TryConsume of an exhausted right = %v, %v", ok, err)
		}
		if err := s.Unconsume(ctx, e.ID, 1, now); err != nil {
			t.Fatal(err)
		}
		if r, _ := s.Get(ctx, org, e.ID); r.QuantityUsed != 1 || r.Status != entitlements.StatusActive {
			t.Fatalf("after Unconsume: %+v", r)
		}
		if err := s.Unconsume(ctx, e.ID, 2, now); !errors.Is(err, entitlements.ErrNotFound) {
			t.Fatalf("Unconsume of more than used: %v", err)
		}
		// An exhausted right given back after its end becomes expired.
		if _, ok, _ := s.TryConsume(ctx, org, e.ID, 1, now); !ok {
			t.Fatal("re-consume failed")
		}
		if err := s.Unconsume(ctx, e.ID, 1, now.Add(2*day)); err != nil {
			t.Fatal(err)
		}
		if r, _ := s.Get(ctx, org, e.ID); r.Status != entitlements.StatusExpired {
			t.Fatalf("status %s, want expired", r.Status)
		}
	})

	t.Run("activate, revoke and expire", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		org, admin := w.org(t), w.user(t)
		ctx := t.Context()
		line := uuid.NewV7()
		pend := quota(org, admin, entitlements.FeatureContactRequest, 3, now.Add(-day), now.Add(day))
		pend.Status, pend.ActivatedAt, pend.SourceType, pend.SourceID, pend.GrantedBy = entitlements.StatusPending, nil,
			entitlements.SourceOrderLine, &line, nil
		insert(t, s, pend)
		if n, err := s.Activate(ctx, entitlements.SourceOrderLine, line, now); n != 1 || err != nil {
			t.Fatalf("Activate = %d, %v", n, err)
		}
		if n, err := s.Activate(ctx, entitlements.SourceOrderLine, line, now); n != 0 || err != nil {
			t.Fatalf("second Activate = %d, %v", n, err)
		}
		if n, err := s.Revoke(ctx, entitlements.SourceOrderLine, line, now, "refund"); n != 1 || err != nil {
			t.Fatalf("Revoke = %d, %v", n, err)
		}
		r, _ := s.Get(ctx, org, pend.ID)
		if r.Status != entitlements.StatusRevoked || r.RevokedAt == nil || r.RevokeReason == nil || *r.RevokeReason != "refund" || r.ActivatedAt == nil {
			t.Fatalf("revoked = %+v", r)
		}
		if n, _ := s.Revoke(ctx, entitlements.SourceOrderLine, line, now, ""); n != 0 {
			t.Fatal("Revoke changed a revoked right")
		}
		one := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 3, now.Add(-day), now.Add(day)))
		got, err := s.RevokeOne(ctx, one.ID, now, "")
		if err != nil || got.Status != entitlements.StatusRevoked || got.RevokeReason != nil {
			t.Fatalf("RevokeOne = %+v, %v", got, err)
		}
		if _, err := s.RevokeOne(ctx, one.ID, now, ""); !errors.Is(err, entitlements.ErrNotFound) {
			t.Fatalf("RevokeOne of a revoked right: %v", err)
		}

		ended := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 3, now.Add(-2*day), now.Add(-time.Hour)))
		atEnd := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 3, now.Add(-2*day), now))
		running := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 3, now.Add(-2*day), now.Add(time.Hour)))
		exp, err := s.Expire(ctx, now, 1)
		if err != nil || len(exp) != 1 || exp[0].ID != ended.ID || exp[0].Status != entitlements.StatusExpired {
			t.Fatalf("Expire(limit 1) = %+v, %v", exp, err)
		}
		exp, err = s.Expire(ctx, now, 10)
		if err != nil || len(exp) != 1 || exp[0].ID != atEnd.ID {
			t.Fatalf("Expire = %+v, %v", exp, err)
		}
		if r, _ := s.Get(ctx, org, running.ID); r.Status != entitlements.StatusActive {
			t.Fatalf("running right %s", r.Status)
		}
	})

	t.Run("usages", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		org, other, admin, actor := w.org(t), w.org(t), w.user(t), w.user(t)
		ctx := t.Context()
		post := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 5, now.Add(-day), now.Add(day)))
		contact := insert(t, s, quota(org, admin, entitlements.FeatureContactRequest, 5, now.Add(-day), now.Add(day)))
		foreign := insert(t, s, quota(other, admin, entitlements.FeatureTargetedPost, 5, now.Add(-day), now.Add(day)))
		ref := entitlements.Ref{Type: entitlements.RefPost, ID: uuid.NewV7()}
		// org_id comes from the entitlement row, not from the caller (06 H-38).
		u, ok, err := s.InsertUsage(ctx, entitlements.Usage{
			ID: uuid.NewV7(), EntitlementID: post.ID, OrgID: other, Quantity: 1, RefType: ref.Type, RefID: ref.ID,
			ActorUserID: &actor, CreatedAt: now,
		})
		if !ok || err != nil || u.OrgID != org || u.EntitlementID != post.ID || u.Quantity != 1 {
			t.Fatalf("InsertUsage = %+v, %v, %v", u, ok, err)
		}
		if _, ok, err := s.InsertUsage(ctx, entitlements.Usage{
			ID: uuid.NewV7(), EntitlementID: foreign.ID, Quantity: 1, RefType: ref.Type, RefID: ref.ID, CreatedAt: now,
		}); ok || err != nil {
			t.Fatalf("second consumption of a ref = %v, %v", ok, err)
		}
		if _, ok, err := s.InsertUsage(ctx, entitlements.Usage{
			ID: uuid.NewV7(), EntitlementID: uuid.NewV7(), Quantity: 1, RefType: entitlements.RefPost, RefID: uuid.NewV7(), CreatedAt: now,
		}); ok || err != nil {
			t.Fatalf("usage of a missing entitlement = %v, %v", ok, err)
		}
		got, reversed, err := s.UsageByRef(ctx, ref)
		if err != nil || reversed || got.ID != u.ID || got.ActorUserID == nil || *got.ActorUserID != actor {
			t.Fatalf("UsageByRef = %+v, %v, %v", got, reversed, err)
		}
		if _, _, err := s.UsageByRef(ctx, entitlements.Ref{Type: entitlements.RefPost, ID: uuid.NewV7()}); !errors.Is(err, entitlements.ErrNotFound) {
			t.Fatalf("UsageByRef of an unknown ref: %v", err)
		}
		// A reversal copies entitlement, org, ref and the negated quantity
		// from the reversed usage, whatever the caller passes (T-134).
		of := u.ID
		r, ok, err := s.InsertUsage(ctx, entitlements.Usage{
			ID: uuid.NewV7(), ReversalOf: &of, EntitlementID: foreign.ID, OrgID: other, Quantity: -5,
			RefType: entitlements.RefDispatch, RefID: uuid.NewV7(), CreatedAt: now.Add(time.Second),
		})
		if !ok || err != nil || r.EntitlementID != post.ID || r.OrgID != org || r.Quantity != -1 ||
			r.RefType != ref.Type || r.RefID != ref.ID || r.ActorUserID != nil {
			t.Fatalf("reversal = %+v, %v, %v", r, ok, err)
		}
		if _, ok, _ := s.InsertUsage(ctx, entitlements.Usage{ID: uuid.NewV7(), ReversalOf: &of, CreatedAt: now}); ok {
			t.Fatal("a second reversal was written")
		}
		rof := r.ID
		if _, ok, _ := s.InsertUsage(ctx, entitlements.Usage{ID: uuid.NewV7(), ReversalOf: &rof, CreatedAt: now}); ok {
			t.Fatal("a reversal of a reversal was written")
		}
		if _, reversed, _ := s.UsageByRef(ctx, ref); !reversed {
			t.Fatal("UsageByRef does not see the reversal")
		}
		cref := entitlements.Ref{Type: entitlements.RefContactRequest, ID: uuid.NewV7()}
		if _, ok, err := s.InsertUsage(ctx, entitlements.Usage{
			ID: uuid.NewV7(), EntitlementID: contact.ID, Quantity: 1, RefType: cref.Type, RefID: cref.ID, CreatedAt: now,
		}); !ok || err != nil {
			t.Fatalf("contact usage = %v, %v", ok, err)
		}
		list, err := s.ListUsages(ctx, org, entitlements.UsageFilter{}, nil, 10)
		if err != nil || len(list) != 3 || list[0].Feature != entitlements.FeatureContactRequest || list[1].Quantity != -1 {
			t.Fatalf("ListUsages = %+v, %v", list, err)
		}
		posts, err := s.ListUsages(ctx, org, entitlements.UsageFilter{Feature: entitlements.FeatureTargetedPost}, nil, 10)
		if err != nil || len(posts) != 2 {
			t.Fatalf("ListUsages(post) = %+v, %v", posts, err)
		}
		after, err := s.ListUsages(ctx, org, entitlements.UsageFilter{}, &list[0].ID, 1)
		if err != nil || len(after) != 1 || after[0].ID != list[1].ID {
			t.Fatalf("ListUsages after = %+v, %v", after, err)
		}
		if none, err := s.ListUsages(ctx, other, entitlements.UsageFilter{}, nil, 10); err != nil || len(none) != 0 {
			t.Fatalf("ListUsages of another org = %+v, %v", none, err)
		}
		if _, err := s.ListUsages(ctx, uuid.Nil(), entitlements.UsageFilter{}, nil, 10); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("ListUsages without org: %v", err)
		}
		// KVKK export (BE-057): the actor's own usages only.
		mine, err := s.UsagesOfActor(ctx, actor)
		if err != nil || len(mine) != 1 || mine[0].ID != u.ID {
			t.Fatalf("UsagesOfActor = %+v, %v", mine, err)
		}
		if none, _ := s.UsagesOfActor(ctx, admin); len(none) != 0 {
			t.Fatalf("UsagesOfActor(admin) = %+v", none)
		}
	})

	t.Run("retention of ended grants (BE-057)", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		org, admin := w.org(t), w.user(t)
		ctx := t.Context()
		// The database clock is real: dates are relative to the real date.
		real := time.Now()
		ended := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 5, real.AddDate(-4, 0, 0), real.AddDate(-3, 0, 0)))
		young := insert(t, s, quota(org, admin, entitlements.FeatureTargetedPost, 5, real.AddDate(-1, 0, 0), real.AddDate(0, -6, 0)))
		order := quota(org, admin, entitlements.FeatureContactRequest, 5, real.AddDate(-4, 0, 0), real.AddDate(-3, 0, 0))
		src := uuid.NewV7()
		order.SourceType, order.SourceID, order.GrantedBy = entitlements.SourceOrderLine, &src, nil
		insert(t, s, order)
		cutoff := real.AddDate(-2, -1, 0)
		if n, err := s.PurgeEndedGrants(ctx, cutoff, 1000, true); err != nil || n != 1 {
			t.Fatalf("dry run = %d, %v; want the admin grant that ended 3 years ago", n, err)
		}
		if _, err := s.Get(ctx, org, ended.ID); err != nil {
			t.Fatalf("a dry run deleted the grant: %v", err)
		}
		if _, err := s.Get(ctx, org, young.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) (entitlements.Store, world) {
		return entitlements.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}
