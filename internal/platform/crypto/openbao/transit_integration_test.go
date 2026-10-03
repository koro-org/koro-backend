//go:build integration

package openbao_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/crypto/openbao"
	"koro/internal/platform/crypto/openbao/openbaotest"
)

// TestOpenBao_SealOpen_KeyIDHeader runs the envelope against a real OpenBao
// (dev container) with a token restricted to the documented minimal
// policy: the Transit key id and version are in the ciphertext header, the
// purpose is bound through Transit associated data, a key rotation moves new
// values to the new version while old values still open, and the periodic
// token renews itself.
func TestOpenBao_SealOpen_KeyIDHeader(t *testing.T) {
	t.Parallel()
	srv := openbaotest.Start(t, "koro-kek")
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	tr, err := openbao.New(openbao.Config{Addr: srv.Addr, Token: srv.AppToken, Key: srv.Key, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := tr.Check(ctx); err != nil {
		t.Fatalf("Check() = %v", err)
	}
	env, _ := crypto.NewEnvelope(tr)
	const purpose = "identity.mfa_secret/0199a4f0-0000-7000-8000-000000000001"
	sealed, err := env.Seal(ctx, purpose, []byte("totp-secret"))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	kid, err := crypto.KeyID(sealed)
	if err != nil || kid != "bao.koro-kek.v1" {
		t.Fatalf("KeyID = %q, %v", kid, err)
	}
	// Header layout: 0x01 ‖ len(kid) ‖ kid ‖ len(wrapped) ‖ "vault:v1:…".
	if sealed[0] != 0x01 || int(sealed[1]) != len(kid) || string(sealed[2:2+len(kid)]) != kid ||
		!bytes.HasPrefix(sealed[4+len(kid):], []byte("vault:v1:")) {
		t.Fatalf("header = %q", sealed[:min(len(sealed), 40)])
	}
	if pt, err := env.Open(ctx, purpose, sealed); err != nil || string(pt) != "totp-secret" {
		t.Fatalf("Open() = %q, %v", pt, err)
	}
	if _, err := env.Open(ctx, purpose+"x", sealed); !errors.Is(err, crypto.ErrDecrypt) {
		t.Fatalf("Open with another purpose = %v, want ErrDecrypt (Transit associated data)", err)
	}

	srv.Rotate(t)
	clk.Advance(2 * time.Minute) // past the active key cache
	if active, err := env.ActiveKeyID(ctx); err != nil || active != "bao.koro-kek.v2" {
		t.Fatalf("ActiveKeyID after rotation = %q, %v", active, err)
	}
	moved, changed, err := env.Rewrap(ctx, purpose, sealed)
	if err != nil || !changed {
		t.Fatalf("Rewrap = %v, %v", changed, err)
	}
	if kid, _ := crypto.KeyID(moved); kid != "bao.koro-kek.v2" {
		t.Fatalf("rewrapped KeyID = %s", kid)
	}
	for _, v := range [][]byte{sealed, moved} {
		if pt, err := env.Open(ctx, purpose, v); err != nil || string(pt) != "totp-secret" {
			t.Fatalf("Open after rotation = %q, %v", pt, err)
		}
	}
	if renewed, err := tr.RenewToken(ctx); err != nil || !renewed {
		t.Fatalf("RenewToken = %v, %v", renewed, err)
	}
}
