package entitlements_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/entitlements"
)

// TestUsage_SumsTheUsableRightsPerFeature (BE-163): every feature in
// Features() order; a quota feature sums the units and the spent units of
// its usable rights (an expired one is left out), discovery_access carries
// the end of the running right; another org's rights do not count.
func TestUsage_SumsTheUsableRightsPerFeature(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, other := uuid.NewV7(), uuid.NewV7()
	now := e.clk.Now()
	e.quota(t, org, entitlements.FeatureTargetedPost, 5)
	e.quota(t, org, entitlements.FeatureTargetedPost, 3)
	e.grant(t, org, entitlements.FeatureTargetedPost, ptr(10), now.AddDate(0, -2, 0), now.AddDate(0, -1, 0)) // expired
	e.quota(t, other, entitlements.FeatureContactRequest, 7)
	until := now.Add(10 * 24 * time.Hour)
	e.grant(t, org, entitlements.FeatureDiscoveryAccess, nil, now.Add(-time.Hour), until)
	for range 2 {
		if _, err := e.svc.Consume(ctx, org, entitlements.FeatureTargetedPost, 1, postRef()); err != nil {
			t.Fatal(err)
		}
	}
	stats, ok := e.svc.(entitlements.Statistics)
	if !ok {
		t.Fatal("the service does not offer entitlements.Statistics")
	}
	got, err := stats.Usage(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(entitlements.Features()) {
		t.Fatalf("usage = %+v, want every feature", got)
	}
	for i, f := range entitlements.Features() {
		u := got[i]
		if u.Feature != f {
			t.Fatalf("usage[%d] = %s, want %s", i, u.Feature, f)
		}
		switch f {
		case entitlements.FeatureTargetedPost:
			if u.Total != 8 || u.Used != 2 || u.Remaining != 6 || u.AccessUntil != nil {
				t.Fatalf("targeted_post = %+v", u)
			}
		case entitlements.FeatureDiscoveryAccess:
			if u.AccessUntil == nil || !u.AccessUntil.Equal(until) || u.Total != 0 {
				t.Fatalf("discovery_access = %+v", u)
			}
		default:
			if u.Total != 0 || u.Used != 0 || u.Remaining != 0 {
				t.Fatalf("%s = %+v, want nothing", f, u)
			}
		}
	}
}
