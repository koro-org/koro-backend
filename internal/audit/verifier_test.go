package audit_test

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"koro/internal/audit"
	"koro/internal/authz/authztest"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
)

// publicKeys returns the Ed25519 public keys of seed keys under their IDs.
func publicKeys(keys []crypto.Key) []crypto.Key {
	out := make([]crypto.Key, 0, len(keys))
	for _, k := range keys {
		pub, _ := ed25519.NewKeyFromSeed(k.Secret).Public().(ed25519.PublicKey)
		out = append(out, crypto.Key{ID: k.ID, Secret: pub})
	}
	return out
}

// The API holds no seed (BE-107): a module with only a public-key verifier
// verifies the roots the worker signed, and without any key it fails loudly.
func TestVerify_APIWithPublicKeysOnly(t *testing.T) {
	t.Parallel()
	worker := newEnv(t, nil)
	ctx := t.Context()
	worker.logN(t, ctx, 3, "identity.session.create", authztest.ID("student"), nil)
	worker.chain(t)
	worker.clk.Set(t0.Add(24 * time.Hour))
	if err := audit.DailyRoots(ctx, worker.m); err != nil {
		t.Fatal(err)
	}

	api := func(v audit.RootVerifier) audit.Module {
		return audit.New(audit.Deps{Store: worker.store, Tx: db.NopTx{}, Clock: worker.clk, Log: obs.Discard(), Verifier: v})
	}
	good, err := audit.NewEd25519Verifier(publicKeys(testKeys))
	if err != nil {
		t.Fatal(err)
	}
	if res, err := audit.VerifyRange(ctx, api(good), day0, day0); err != nil || !res.OK {
		t.Fatalf("VerifyRange() with public keys = %+v, %v", res, err)
	}
	other, err := audit.NewEd25519Verifier(publicKeys([]crypto.Key{{ID: "test1", Secret: []byte("another-audit-test-root-seed-32b")}}))
	if err != nil {
		t.Fatal(err)
	}
	if res, err := audit.VerifyRange(ctx, api(other), day0, day0); err != nil || res.OK || res.Problem != "signature" {
		t.Fatalf("VerifyRange() with a foreign public key = %+v, %v; want a signature finding", res, err)
	}
	if _, err := audit.VerifyRange(ctx, api(nil), day0, day0); !errors.Is(err, audit.ErrSignerNotConfigured) {
		t.Fatalf("VerifyRange() without keys error = %v", err)
	}
	if err := audit.DailyRoots(ctx, api(good)); !errors.Is(err, audit.ErrSignerNotConfigured) {
		t.Fatalf("DailyRoots() with only a verifier error = %v; want signer_not_configured", err)
	}
}

func TestEd25519Verifier_KeysAndCoverage(t *testing.T) {
	t.Parallel()
	seeds := []crypto.Key{testKeys[0], {ID: "old", Secret: []byte("old-audit-test-root-seed-32bytes")}}
	signer, err := audit.NewEd25519Signer(seeds)
	if err != nil {
		t.Fatal(err)
	}
	pubs := publicKeys(seeds)
	full, err := audit.NewEd25519Verifier(pubs)
	if err != nil {
		t.Fatal(err)
	}
	if err := full.CheckCovers(signer); err != nil {
		t.Fatalf("CheckCovers() = %v", err)
	}
	missing, _ := audit.NewEd25519Verifier(pubs[:1])
	if err := missing.CheckCovers(signer); err == nil || !strings.Contains(err.Error(), "signing key old has no") {
		t.Fatalf("CheckCovers() missing = %v", err)
	}
	swapped, _ := audit.NewEd25519Verifier([]crypto.Key{{ID: "test1", Secret: pubs[1].Secret}, {ID: "old", Secret: pubs[1].Secret}})
	if err := swapped.CheckCovers(signer); err == nil || !strings.Contains(err.Error(), "not the public key of signing key test1") {
		t.Fatalf("CheckCovers() swapped = %v", err)
	}
	for _, bad := range [][]crypto.Key{nil, {{ID: "k", Secret: []byte("short")}}, {pubs[0], pubs[0]}} {
		if v, err := audit.NewEd25519Verifier(bad); err == nil || v != nil {
			t.Fatalf("NewEd25519Verifier(%d keys) = %v, nil; want an error", len(bad), v)
		}
	}
}
