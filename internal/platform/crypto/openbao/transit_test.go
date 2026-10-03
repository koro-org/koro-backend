package openbao_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/crypto/openbao"
	"koro/internal/platform/crypto/openbao/openbaotest"
)

func newTransit(t *testing.T, addr, token string, clk clock.Clock) *openbao.Transit {
	t.Helper()
	tr, err := openbao.New(openbao.Config{Addr: addr, Token: token, Key: "koro-kek", Clock: clk, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func localKEK(t *testing.T) *crypto.LocalKEK {
	t.Helper()
	k, err := crypto.NewLocalKEK([]crypto.Key{{ID: "k1", Secret: bytes.Repeat([]byte{7}, crypto.KeySize)}})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The envelope over Transit: the key id is in the header, the purpose is
// bound, rotation is visible to ActiveKeyID and old values still open.
func TestTransit_FakeRoundTrip(t *testing.T) {
	t.Parallel()
	f := openbaotest.NewFake(t, "koro-kek")
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	tr := newTransit(t, f.Server.URL, openbaotest.FakeToken, clk)
	env, err := crypto.NewEnvelope(tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := tr.Check(ctx); err != nil {
		t.Fatalf("Check() = %v", err)
	}
	sealed, err := env.Seal(ctx, "identity.mfa_secret/u1", []byte("JBSWY3DPEHPK3PXP"))
	if err != nil {
		t.Fatal(err)
	}
	if kid, _ := crypto.KeyID(sealed); kid != "bao.koro-kek.v1" || !tr.Owns(kid) {
		t.Fatalf("KeyID = %q, want bao.koro-kek.v1", kid)
	}
	if bytes.Contains(sealed, []byte("JBSWY3DPEHPK3PXP")) {
		t.Fatal("plaintext visible in the envelope")
	}
	pt, err := env.Open(ctx, "identity.mfa_secret/u1", sealed)
	if err != nil || string(pt) != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("Open = %q, %v", pt, err)
	}
	if _, err := env.Open(ctx, "identity.mfa_secret/u2", sealed); !errors.Is(err, crypto.ErrDecrypt) {
		t.Fatalf("Open with another purpose = %v, want ErrDecrypt", err)
	}

	f.Rotate("koro-kek")
	if kid, _ := tr.ActiveKeyID(ctx); kid != "bao.koro-kek.v1" {
		t.Fatalf("active key changed before the cache expired: %s", kid)
	}
	clk.Advance(2 * time.Minute)
	if kid, _ := tr.ActiveKeyID(ctx); kid != "bao.koro-kek.v2" {
		t.Fatalf("ActiveKeyID after rotation = %s", kid)
	}
	again, changed, err := env.Rewrap(ctx, "identity.mfa_secret/u1", sealed)
	if err != nil || !changed {
		t.Fatalf("Rewrap = %v, %v", changed, err)
	}
	if kid, _ := crypto.KeyID(again); kid != "bao.koro-kek.v2" {
		t.Fatalf("rewrapped KeyID = %s", kid)
	}
	if _, changed, _ := env.Rewrap(ctx, "identity.mfa_secret/u1", again); changed {
		t.Fatal("Rewrap of a current value changed it")
	}
	if pt, err := env.Open(ctx, "identity.mfa_secret/u1", sealed); err != nil || string(pt) != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("v1 value after rotation = %q, %v", pt, err)
	}

	// A header whose key id does not match the Transit ciphertext version
	// is refused before OpenBao is asked.
	if _, err := tr.Unwrap(ctx, "bao.koro-kek.v1", []byte("vault:v2:AAAA"), nil); !errors.Is(err, crypto.ErrDecrypt) {
		t.Fatalf("mismatched version = %v", err)
	}
	for _, kid := range []string{"k1", "bao.other.v1", "bao.koro-kek.v0", "bao.koro-kek.v01", "bao.koro-kek.v"} {
		if tr.Owns(kid) {
			t.Errorf("Owns(%q) = true", kid)
		}
	}
}

// Fail closed: with OpenBao down nothing is sealed (no local fallback) and
// nothing opens; the token never appears in an error.
func TestTransit_Outage_FailsClosed(t *testing.T) {
	t.Parallel()
	f := openbaotest.NewFake(t, "koro-kek")
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	tr := newTransit(t, f.Server.URL, openbaotest.FakeToken, clk)
	local := localKEK(t)
	chain, err := crypto.NewChain(tr, local)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := crypto.NewEnvelope(chain)
	ctx := t.Context()
	baoValue, err := env.Seal(ctx, "p", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	localEnv, _ := crypto.NewEnvelope(local)
	localValue, _ := localEnv.Seal(ctx, "p", []byte("old"))

	f.SetDown(true)
	out, err := env.Seal(ctx, "p", []byte("secret"))
	if out != nil || !errors.Is(err, crypto.ErrUnavailable) {
		t.Fatalf("Seal during an outage = %x, %v; want no output and ErrUnavailable", out, err)
	}
	if _, err := env.Open(ctx, "p", baoValue); !errors.Is(err, crypto.ErrUnavailable) {
		t.Fatalf("Open during an outage = %v", err)
	}
	if err := tr.Check(ctx); !errors.Is(err, crypto.ErrUnavailable) {
		t.Fatalf("Check during an outage = %v", err)
	}
	// Values of the local KEK still open (reading old data needs no OpenBao).
	if pt, err := env.Open(ctx, "p", localValue); err != nil || string(pt) != "old" {
		t.Fatalf("local value during an outage = %q, %v", pt, err)
	}
	f.SetDown(false)

	bad := newTransit(t, f.Server.URL, "wrong-token-value-123", clk)
	_, _, err = bad.Wrap(ctx, make([]byte, 32), []byte("aad"))
	if !errors.Is(err, crypto.ErrUnavailable) || strings.Contains(err.Error(), "wrong-token-value-123") {
		t.Fatalf("Wrap with a bad token = %v", err)
	}
	unreachable := newTransit(t, "http://127.0.0.1:1", openbaotest.FakeToken, clk)
	if _, _, err := unreachable.Wrap(ctx, make([]byte, 32), nil); !errors.Is(err, crypto.ErrUnavailable) ||
		strings.Contains(err.Error(), openbaotest.FakeToken) {
		t.Fatalf("Wrap against a closed port = %v", err)
	}
}

func TestTransit_ConfigAndRenew(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	for _, c := range []openbao.Config{
		{Addr: "", Token: "t", Clock: clk},
		{Addr: "ftp://bao", Token: "t", Clock: clk},
		{Addr: "https://user:pw@bao", Token: "t", Clock: clk},
		{Addr: "https://bao", Token: "", Clock: clk},
		{Addr: "https://bao", Token: "t", Key: "bad key", Clock: clk},
		{Addr: "https://bao", Token: "t", Key: strings.Repeat("k", 51), Clock: clk},
		{Addr: "https://bao", Token: "t", Mount: "../sys", Clock: clk},
		{Addr: "https://bao", Token: "t"},
		{Addr: "https://bao", Token: "t", Clock: clk, CACertPEM: []byte("not pem")},
	} {
		if _, err := openbao.New(c); !errors.Is(err, crypto.ErrConfig) {
			t.Errorf("New(%+v) = %v, want ErrConfig", c.Addr+"|"+c.Key+"|"+c.Mount, err)
		}
	}
	f := openbaotest.NewFake(t, "koro-kek")
	tr := newTransit(t, f.Server.URL, openbaotest.FakeToken, clk)
	if renewed, err := tr.RenewToken(t.Context()); err != nil || !renewed || f.Renews.Load() != 1 {
		t.Fatalf("RenewToken = %v, %v (renews %d)", renewed, err, f.Renews.Load())
	}
	missing, _ := openbao.New(openbao.Config{Addr: f.Server.URL, Token: openbaotest.FakeToken, Key: "absent", Clock: clk})
	if err := missing.Check(t.Context()); err == nil {
		t.Fatal("Check of a missing key succeeded")
	}
}
