package crypto_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
)

func key(fill byte) []byte { return bytes.Repeat([]byte{fill}, crypto.KeySize) }

// cryptoOnly loads just the crypto section, independent of other tickets'
// required settings.
type cryptoOnly struct {
	config.Common
	Crypto config.Crypto
}

func newEnvelope(t *testing.T, keys ...crypto.Key) *crypto.Envelope {
	t.Helper()
	kek, err := crypto.NewLocalKEK(keys)
	if err != nil {
		t.Fatal(err)
	}
	env, err := crypto.NewEnvelope(kek)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestCrypto_SealOpen_PurposeBound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	env := newEnvelope(t, crypto.Key{ID: "k1", Secret: key(1)})
	secret := []byte("JBSWY3DPEHPK3PXP") // synthetic TOTP secret

	sealed, err := env.Seal(ctx, "identity.mfa_secret", secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, secret) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if kid, err := crypto.KeyID(sealed); err != nil || kid != "k1" {
		t.Fatalf("KeyID = %q, %v", kid, err)
	}
	got, err := env.Open(ctx, "identity.mfa_secret", sealed)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("Open = %q, %v", got, err)
	}
	again, _ := env.Seal(ctx, "identity.mfa_secret", secret)
	if bytes.Equal(again, sealed) {
		t.Fatal("two seals of the same value are identical (DEK/nonce reuse)")
	}

	for _, purpose := range []string{"identity.mfa_secre", "identity.mfa_secret ", "parties.tckn", "IDENTITY.MFA_SECRET"} {
		if _, err := env.Open(ctx, purpose, sealed); !errors.Is(err, crypto.ErrDecrypt) {
			t.Errorf("Open(purpose %q) error = %v, want ErrDecrypt", purpose, err)
		}
	}
	if _, err := env.Seal(ctx, "", secret); !errors.Is(err, crypto.ErrPurpose) {
		t.Fatalf("Seal(empty purpose) error = %v", err)
	}
	if _, err := env.Open(ctx, "", sealed); !errors.Is(err, crypto.ErrPurpose) {
		t.Fatalf("Open(empty purpose) error = %v", err)
	}

	// Every single-byte modification is detected.
	for i := range sealed {
		tampered := bytes.Clone(sealed)
		tampered[i] ^= 0x01
		if pt, err := env.Open(ctx, "identity.mfa_secret", tampered); err == nil {
			t.Fatalf("byte %d modified: Open succeeded with %q", i, pt)
		}
	}
	for _, short := range [][]byte{nil, {}, {1}, sealed[:len(sealed)-1], sealed[:20]} {
		if _, err := env.Open(ctx, "identity.mfa_secret", short); err == nil {
			t.Fatalf("truncated ciphertext (%d bytes) opened", len(short))
		}
	}

	// Another KEK with the same id cannot open it.
	other := newEnvelope(t, crypto.Key{ID: "k1", Secret: key(2)})
	if _, err := other.Open(ctx, "identity.mfa_secret", sealed); !errors.Is(err, crypto.ErrDecrypt) {
		t.Fatalf("Open with another KEK error = %v", err)
	}

	// Empty plaintext round-trips.
	empty, err := env.Seal(ctx, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := env.Open(ctx, "x", empty); err != nil || len(pt) != 0 {
		t.Fatalf("Open(empty) = %q, %v", pt, err)
	}
}

func TestCrypto_KeyRotation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	oldEnv := newEnvelope(t, crypto.Key{ID: "2026a", Secret: key(1)})
	sealedOld, err := oldEnv.Seal(ctx, "p", []byte("eski"))
	if err != nil {
		t.Fatal(err)
	}
	rotated := newEnvelope(t, crypto.Key{ID: "2026b", Secret: key(2)}, crypto.Key{ID: "2026a", Secret: key(1)})
	if pt, err := rotated.Open(ctx, "p", sealedOld); err != nil || string(pt) != "eski" {
		t.Fatalf("Open(old KEK) after rotation = %q, %v", pt, err)
	}
	sealedNew, err := rotated.Seal(ctx, "p", []byte("yeni"))
	if err != nil {
		t.Fatal(err)
	}
	if kid, _ := crypto.KeyID(sealedNew); kid != "2026b" {
		t.Fatalf("new seal uses %q, want the active key 2026b", kid)
	}
	if _, err := oldEnv.Open(ctx, "p", sealedNew); !errors.Is(err, crypto.ErrUnknownKey) {
		t.Fatalf("Open(unknown kid) error = %v, want ErrUnknownKey", err)
	}
}

// failingWrapper simulates a remote KMS outage.
type failingWrapper struct{ err error }

func (f failingWrapper) Wrap(context.Context, []byte, []byte) (string, []byte, error) {
	return "", nil, f.err
}

func (f failingWrapper) Unwrap(context.Context, string, []byte, []byte) ([]byte, error) {
	return nil, f.err
}

func TestCrypto_WrapperErrorsPropagate(t *testing.T) {
	t.Parallel()
	outage := errors.New("kms unavailable")
	env, err := crypto.NewEnvelope(failingWrapper{err: outage})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Seal(t.Context(), "p", []byte("x")); !errors.Is(err, outage) {
		t.Fatalf("Seal error = %v", err)
	}
	sealed, _ := newEnvelope(t, crypto.Key{ID: "k1", Secret: key(1)}).Seal(t.Context(), "p", []byte("x"))
	if _, err := env.Open(t.Context(), "p", sealed); !errors.Is(err, outage) || errors.Is(err, crypto.ErrDecrypt) {
		t.Fatalf("Open error = %v, want the wrapper error", err)
	}
}

func TestCrypto_BlindIndex_Deterministic(t *testing.T) {
	t.Parallel()
	x, err := crypto.NewIndexer(key(7))
	if err != nil {
		t.Fatal(err)
	}
	a := x.BlindIndex("identity.email", []byte("ayse@example.test"))
	b := x.BlindIndex("identity.email", []byte("ayse@example.test"))
	if len(a) != 32 || !hmac.Equal(a, b) {
		t.Fatalf("BlindIndex is not deterministic: %x vs %x", a, b)
	}
	same, _ := crypto.NewIndexer(key(7))
	if !hmac.Equal(a, same.BlindIndex("identity.email", []byte("ayse@example.test"))) {
		t.Fatal("BlindIndex differs across instances with the same key (restart would break lookups)")
	}
	distinct := [][]byte{
		x.BlindIndex("identity.email", []byte("Ayse@example.test")),
		x.BlindIndex("parties.email", []byte("ayse@example.test")),
		x.BlindIndex("identity.email", nil),
	}
	other, _ := crypto.NewIndexer(key(8))
	distinct = append(distinct, other.BlindIndex("identity.email", []byte("ayse@example.test")))
	for i, d := range distinct {
		if hmac.Equal(a, d) {
			t.Errorf("case %d collides with the original index", i)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("BlindIndex with an empty purpose did not panic")
			}
		}()
		x.BlindIndex("", []byte("x"))
	}()
	if _, err := crypto.NewIndexer(make([]byte, 31)); !errors.Is(err, crypto.ErrConfig) {
		t.Fatalf("NewIndexer(31 bytes) error = %v", err)
	}
}

func TestCrypto_DerivedToken(t *testing.T) {
	t.Parallel()
	d, err := crypto.NewDeriver([]crypto.Key{{ID: "q2", Secret: key(3)}, {ID: "q1", Secret: key(4)}})
	if err != nil {
		t.Fatal(err)
	}
	if d.ActiveKeyID() != "q2" {
		t.Fatalf("ActiveKeyID = %q", d.ActiveKeyID())
	}
	t1, err := d.DerivedToken("events.qr", "q2", []byte("registration-1"), []byte{1})
	if err != nil || len(t1) != 32 {
		t.Fatalf("DerivedToken = %x, %v", t1, err)
	}
	t2, _ := d.DerivedToken("events.qr", "q2", []byte("registration-1"), []byte{1})
	if !hmac.Equal(t1, t2) {
		t.Fatal("DerivedToken is not deterministic")
	}
	for name, f := range map[string]func() ([]byte, error){
		"old key": func() ([]byte, error) { return d.DerivedToken("events.qr", "q1", []byte("registration-1"), []byte{1}) },
		"version": func() ([]byte, error) { return d.DerivedToken("events.qr", "q2", []byte("registration-1"), []byte{2}) },
		"purpose": func() ([]byte, error) {
			return d.DerivedToken("events.invite", "q2", []byte("registration-1"), []byte{1})
		},
		"part framing": func() ([]byte, error) {
			return d.DerivedToken("events.qr", "q2", []byte("registration-"), []byte("1"), []byte{1})
		},
	} {
		got, err := f()
		if err != nil || hmac.Equal(got, t1) {
			t.Errorf("%s: token not distinct (%v)", name, err)
		}
	}
	if _, err := d.DerivedToken("events.qr", "q9", []byte("x")); !errors.Is(err, crypto.ErrUnknownKey) {
		t.Fatalf("unknown key id error = %v", err)
	}
	if _, err := d.DerivedToken("", "q2", []byte("x")); !errors.Is(err, crypto.ErrPurpose) {
		t.Fatalf("empty purpose error = %v", err)
	}
}

func TestCrypto_MissingKEK_FailsFast(t *testing.T) {
	t.Parallel()
	bad := [][]crypto.Key{
		nil,
		{},
		{{ID: "k1", Secret: make([]byte, 16)}},
		{{ID: "k1", Secret: key(1)}, {ID: "k1", Secret: key(2)}},
		{{ID: "bad id", Secret: key(1)}},
	}
	for i, keys := range bad {
		if kek, err := crypto.NewLocalKEK(keys); kek != nil || !errors.Is(err, crypto.ErrConfig) {
			t.Errorf("case %d: NewLocalKEK = %v, %v; want ErrConfig", i, kek, err)
		}
	}
	if env, err := crypto.NewEnvelope(nil); env != nil || !errors.Is(err, crypto.ErrConfig) {
		t.Fatalf("NewEnvelope(nil) = %v, %v", env, err)
	}

	// The process configuration refuses to load without a usable KEK.
	const keks = "dev1:ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGU="
	base := func(kv ...string) map[string]string {
		m := map[string]string{
			"APP_ENV":                "dev",
			"CRYPTO_KEKS":            keks,
			"CRYPTO_BLIND_INDEX_KEY": "aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWQ=",
			"CRYPTO_TOKEN_KEYS":      "dev1:dG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG8=",
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "<unset>" {
				delete(m, kv[i])
				continue
			}
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	if _, err := config.Load[cryptoOnly](base()); err != nil {
		t.Fatalf("valid crypto config rejected: %v", err)
	}
	// With OpenBao the local KEK is optional (after crypto.rewrap it is
	// removed); prod may opt out of OpenBao only explicitly.
	cfg, err := config.Load[cryptoOnly](base("CRYPTO_KEKS", "<unset>", "CRYPTO_OPENBAO_ADDR", "http://127.0.0.1:8200",
		"CRYPTO_OPENBAO_TOKEN", "dev-only-root-token"))
	if err != nil || !cfg.Crypto.OpenBaoEnabled() || cfg.Crypto.OpenBaoKey != "koro-kek" || cfg.Crypto.OpenBaoMount != "transit" {
		t.Fatalf("OpenBao without local KEK = %+v, %v", cfg.Crypto, err)
	}
	if _, err := config.Load[cryptoOnly](base("APP_ENV", "prod", "CRYPTO_OPENBAO_REQUIRED", "false")); err == nil ||
		strings.Contains(err.Error(), "CRYPTO_OPENBAO_ADDR is required") {
		t.Fatalf("prod with explicit opt-out = %v; want only the placeholder error", err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "crypto_keks")
	if err := os.WriteFile(file, []byte(keks+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load[cryptoOnly](base("CRYPTO_KEKS", "<unset>", "CRYPTO_KEKS_FILE", file)); err != nil {
		t.Fatalf("CRYPTO_KEKS_FILE rejected: %v", err)
	}
	const secretB64 = "c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0MTI="
	tests := []struct {
		name    string
		environ map[string]string
		want    string
	}{
		{"unset", base("CRYPTO_KEKS", "<unset>"), "CRYPTO_KEKS is required unless CRYPTO_OPENBAO_ADDR is set"},
		{"empty", base("CRYPTO_KEKS", ""), "CRYPTO_KEKS is required unless CRYPTO_OPENBAO_ADDR is set"},
		{"secret file missing", base("CRYPTO_KEKS", "<unset>", "CRYPTO_KEKS_FILE", filepath.Join(dir, "absent")), "CRYPTO_KEKS_FILE"},
		{"short key", base("CRYPTO_KEKS", "k1:c2hvcnQ="), "CRYPTO_KEKS"},
		{"no id", base("CRYPTO_KEKS", secretB64), "CRYPTO_KEKS"},
		{"bad base64", base("CRYPTO_KEKS", "k1:"+secretB64[:20]+"!!"), "CRYPTO_KEKS"},
		{"placeholder in prod", base("APP_ENV", "prod"), "looks like a placeholder"},
		{"blind index key short", base("CRYPTO_BLIND_INDEX_KEY", "c2hvcnQ="), "CRYPTO_BLIND_INDEX_KEY"},
		{"token keys missing", base("CRYPTO_TOKEN_KEYS", "<unset>"), "CRYPTO_TOKEN_KEYS"},
		{"reserved OpenBao key id", base("CRYPTO_KEKS", "bao.k1:"+secretB64), "reserved prefix"},
		{"prod requires OpenBao", base("APP_ENV", "prod"), "CRYPTO_OPENBAO_ADDR is required in prod"},
		{"OpenBao required in dev", base("CRYPTO_OPENBAO_REQUIRED", "true"), "CRYPTO_OPENBAO_ADDR is required in dev"},
		{"bad required flag", base("CRYPTO_OPENBAO_REQUIRED", "maybe"), "CRYPTO_OPENBAO_REQUIRED must be true or false"},
		{"OpenBao without token", base("CRYPTO_OPENBAO_ADDR", "http://127.0.0.1:8200"), "openbao token is empty"},
		{"OpenBao over http in staging", base("APP_ENV", "staging", "CRYPTO_OPENBAO_ADDR", "http://bao:8200",
			"CRYPTO_OPENBAO_TOKEN", "s.Q8yJ2mZ5bW1tK4xR7nV0cL3h"), "CRYPTO_OPENBAO_ADDR must use https"},
		{"OpenBao placeholder token in prod", base("APP_ENV", "prod", "CRYPTO_OPENBAO_ADDR", "https://bao:8200",
			"CRYPTO_OPENBAO_TOKEN", "devdevdevdev"), "CRYPTO_OPENBAO_TOKEN looks like a placeholder"},
		{"OpenBao key name", base("CRYPTO_OPENBAO_ADDR", "http://127.0.0.1:8200", "CRYPTO_OPENBAO_TOKEN", "dev-root",
			"CRYPTO_OPENBAO_KEY", "koro kek"), "invalid characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load[cryptoOnly](tt.environ)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want it to mention %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secretB64[:20]) || strings.Contains(err.Error(), "ZGV2ZGV2") ||
				strings.Contains(err.Error(), "Q8yJ2mZ5bW1tK4xR7nV0cL3h") || strings.Contains(err.Error(), "devdevdevdev") {
				t.Fatalf("error leaks key material: %v", err)
			}
		})
	}
}

func TestCrypto_ParseKeyring(t *testing.T) {
	t.Parallel()
	keys, err := crypto.ParseKeyring(" k2:" + "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=" + ",\nk1:AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=\n")
	if err != nil || len(keys) != 2 || keys[0].ID != "k2" || !bytes.Equal(keys[1].Secret, key(1)) {
		t.Fatalf("ParseKeyring = %+v, %v", keys, err)
	}
	for _, bad := range []string{"", " , ", "k1", ":AQEB", "k1:AQEB$", "a:b:c", "ü:AQEBAQ=="} {
		if _, err := crypto.ParseKeyring(bad); !errors.Is(err, crypto.ErrConfig) {
			t.Errorf("ParseKeyring(%q) error = %v, want ErrConfig", bad, err)
		}
	}
}

func FuzzCrypto_Open(f *testing.F) {
	kek, err := crypto.NewLocalKEK([]crypto.Key{{ID: "k1", Secret: key(1)}})
	if err != nil {
		f.Fatal(err)
	}
	env, _ := crypto.NewEnvelope(kek)
	sealed, _ := env.Seal(f.Context(), "p", []byte("hello"))
	f.Add(sealed)
	f.Add([]byte{1, 2, 'k', '1', 0, 1})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		// Must never panic. Seeds are sealed per process with the same KEK,
		// so a successful Open is legitimate; it must then name that KEK.
		_, err := env.Open(t.Context(), "p", b)
		kid, kerr := crypto.KeyID(b)
		if err == nil && (kerr != nil || kid != "k1") {
			t.Fatalf("opened a ciphertext without a valid header: %x", b)
		}
	})
}

// Chain (BE-055): new values only under the active wrapper, old ones open
// through the wrapper that owns their key id; Rewrap moves a value to the
// active key once and is a no-op afterwards.
func TestCrypto_Chain_RoutesByKeyID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	oldKEK, _ := crypto.NewLocalKEK([]crypto.Key{{ID: "pilot1", Secret: key(1)}})
	newKEK, _ := crypto.NewLocalKEK([]crypto.Key{{ID: "k2", Secret: key(2)}})
	oldEnv, _ := crypto.NewEnvelope(oldKEK)
	old, _ := oldEnv.Seal(ctx, "p", []byte("v"))

	chain, err := crypto.NewChain(newKEK, oldKEK)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := crypto.NewEnvelope(chain)
	fresh, _ := env.Seal(ctx, "p", []byte("v"))
	if kid, _ := crypto.KeyID(fresh); kid != "k2" {
		t.Fatalf("new value under %s, want the active k2", kid)
	}
	if pt, err := env.Open(ctx, "p", old); err != nil || string(pt) != "v" {
		t.Fatalf("old value = %q, %v", pt, err)
	}
	if !chain.Owns("pilot1") || !chain.Owns("k2") || chain.Owns("k3") {
		t.Fatal("Owns does not reflect the chained keys")
	}
	moved, changed, err := env.Rewrap(ctx, "p", old)
	if err != nil || !changed {
		t.Fatalf("Rewrap = %v, %v", changed, err)
	}
	if kid, _ := crypto.KeyID(moved); kid != "k2" {
		t.Fatalf("rewrapped under %s", kid)
	}
	if same, changed, err := env.Rewrap(ctx, "p", moved); err != nil || changed || !bytes.Equal(same, moved) {
		t.Fatalf("second Rewrap = %v, %v", changed, err)
	}
	if _, _, err := env.Rewrap(ctx, "q", old); !errors.Is(err, crypto.ErrDecrypt) {
		t.Fatalf("Rewrap with the wrong purpose = %v", err)
	}
	other, _ := crypto.NewLocalKEK([]crypto.Key{{ID: "zz", Secret: key(3)}})
	otherEnv, _ := crypto.NewEnvelope(other)
	foreign, _ := otherEnv.Seal(ctx, "p", []byte("v"))
	if _, err := env.Open(ctx, "p", foreign); !errors.Is(err, crypto.ErrUnknownKey) {
		t.Fatalf("unknown key id = %v", err)
	}

	if _, err := crypto.NewChain(nil); !errors.Is(err, crypto.ErrConfig) {
		t.Fatalf("NewChain(nil) = %v", err)
	}
	if _, err := crypto.NewChain(failingWrapper{}); !errors.Is(err, crypto.ErrConfig) {
		t.Fatalf("NewChain(non-owner) = %v", err)
	}
	plain, _ := crypto.NewEnvelope(failingWrapper{})
	if _, _, err := plain.Rewrap(ctx, "p", old); !errors.Is(err, crypto.ErrConfig) {
		t.Fatalf("Rewrap without an active key = %v", err)
	}
}
