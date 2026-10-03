package passwd_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"koro/internal/platform/passwd"
)

var testPepper = []byte("test-pepper-0123456789abcdef-0123456789")

// hasher is shared: every argon2id run costs 19 MiB and is slow under -race.
var hasher = sync.OnceValue(func() *passwd.Hasher {
	h, err := passwd.New(testPepper, 4)
	if err != nil {
		panic(err)
	}
	return h
})

func TestPasswd_WrongPassword(t *testing.T) {
	t.Parallel()
	h := hasher()
	ctx := t.Context()
	enc, err := h.Hash(ctx, "doğru-Parola-123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=19456,t=2,p=1$") || passwd.NeedsRehash(enc) {
		t.Fatalf("unexpected encoding %q", enc)
	}
	if again, _ := h.Hash(ctx, "doğru-Parola-123"); again == enc {
		t.Fatal("two hashes of the same password are equal (salt missing)")
	}

	ok, rehash, err := h.Verify(ctx, "doğru-Parola-123", enc)
	if !ok || rehash || err != nil {
		t.Fatalf("Verify(correct) = %v, %v, %v", ok, rehash, err)
	}
	// The NFKC form of the password verifies too (fullwidth digits, ligature).
	if ok, _, err := h.Verify(ctx, "doğru-Parola-１２３", enc); !ok || err != nil {
		t.Fatalf("Verify(NFKC-equivalent) = %v, %v", ok, err)
	}
	for _, wrong := range []string{"doğru-parola-123", "dogru-Parola-123", "doğru-Parola-1234", "", "doğru-Parola-123 "} {
		ok, rehash, err := h.Verify(ctx, wrong, enc)
		if ok || rehash || err != nil {
			t.Errorf("Verify(%q) = %v, %v, %v; want false", wrong, ok, rehash, err)
		}
	}

	other, err := passwd.New([]byte("another-pepper-0123456789abcdef-012345"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := other.Verify(ctx, "doğru-Parola-123", enc); ok || err != nil {
		t.Fatalf("Verify with another pepper = %v, %v; want false", ok, err)
	}

	if err := h.VerifyDummy(ctx, "anything"); err != nil {
		t.Fatalf("VerifyDummy error = %v", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.Hash(canceled, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Hash(canceled) error = %v", err)
	}
	if _, _, err := h.Verify(canceled, "x", enc); !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify(canceled) error = %v", err)
	}
}

func TestPasswd_OldParams_NeedsRehash(t *testing.T) {
	t.Parallel()
	h := hasher()
	ctx := t.Context()
	old, err := passwd.HashWithParams(h, "eski-parola-42", 8*1024, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(old, "$m=8192,t=1,p=1$") || !passwd.NeedsRehash(old) {
		t.Fatalf("old hash %q not detected", old)
	}
	ok, rehash, err := h.Verify(ctx, "eski-parola-42", old)
	if !ok || !rehash || err != nil {
		t.Fatalf("Verify(old params) = %v, %v, %v; want ok and needsRehash", ok, rehash, err)
	}
	ok, rehash, err = h.Verify(ctx, "yanlış", old)
	if ok || rehash || err != nil {
		t.Fatalf("Verify(wrong, old params) = %v, %v, %v; want no rehash for a failed login", ok, rehash, err)
	}
	fresh, err := h.Hash(ctx, "eski-parola-42")
	if err != nil || passwd.NeedsRehash(fresh) {
		t.Fatalf("fresh hash %q still needs rehash (%v)", fresh, err)
	}
	if !passwd.NeedsRehash("garbage") {
		t.Fatal("NeedsRehash(garbage) = false")
	}
}

func TestPasswd_CorruptEncoding(t *testing.T) {
	t.Parallel()
	h := hasher()
	const salt, key = "c2FsdHNhbHRzYWx0c2FsdA", "a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2U"
	tests := []struct{ name, enc string }{
		{"empty", ""},
		{"bcrypt", "$2a$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ01234"},
		{"argon2i", "$argon2i$v=19$m=19456,t=2,p=1$" + salt + "$" + key},
		{"version 16", "$argon2id$v=16$m=19456,t=2,p=1$" + salt + "$" + key},
		{"missing part", "$argon2id$v=19$m=19456,t=2,p=1$" + salt},
		{"extra part", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key + "$x"},
		{"no leading $", "argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key},
		{"params reordered", "$argon2id$v=19$t=2,m=19456,p=1$" + salt + "$" + key},
		{"leading zero", "$argon2id$v=19$m=019456,t=2,p=1$" + salt + "$" + key},
		{"plus sign", "$argon2id$v=19$m=+19456,t=2,p=1$" + salt + "$" + key},
		{"missing param", "$argon2id$v=19$m=19456,t=2$" + salt + "$" + key},
		{"memory bomb 4 GiB", "$argon2id$v=19$m=4194304,t=2,p=1$" + salt + "$" + key},
		{"time bomb", "$argon2id$v=19$m=19456,t=1000,p=1$" + salt + "$" + key},
		{"zero time", "$argon2id$v=19$m=19456,t=0,p=1$" + salt + "$" + key},
		{"zero threads", "$argon2id$v=19$m=19456,t=2,p=0$" + salt + "$" + key},
		{"too many threads", "$argon2id$v=19$m=19456,t=2,p=255$" + salt + "$" + key},
		{"memory below 8×p", "$argon2id$v=19$m=7,t=2,p=1$" + salt + "$" + key},
		{"uint32 overflow", "$argon2id$v=19$m=4294967296,t=2,p=1$" + salt + "$" + key},
		{"padded salt", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "==$" + key},
		{"bad base64 salt", "$argon2id$v=19$m=19456,t=2,p=1$***$" + key},
		{"short salt", "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$" + key},
		{"short key", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$a2V5"},
		{"empty key", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$"},
	}
	// Control: the same pieces in canonical form parse (and simply do not match).
	if ok, _, err := h.Verify(t.Context(), "pw", "$argon2id$v=19$m=19456,t=2,p=1$"+salt+"$"+key); ok || err != nil {
		t.Fatalf("well-formed control = %v, %v; want false, nil", ok, err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ok, rehash, err := h.Verify(t.Context(), "pw", tt.enc)
			if ok || rehash || !errors.Is(err, passwd.ErrInvalidHash) {
				t.Fatalf("Verify(%q) = %v, %v, %v; want ErrInvalidHash", tt.enc, ok, rehash, err)
			}
		})
	}
}

func TestPasswd_NewRejectsBadConfig(t *testing.T) {
	t.Parallel()
	if _, err := passwd.New([]byte("short"), 1); !errors.Is(err, passwd.ErrConfig) {
		t.Fatalf("New(short pepper) error = %v", err)
	}
	if _, err := passwd.New(testPepper, 0); !errors.Is(err, passwd.ErrConfig) {
		t.Fatalf("New(maxConcurrent=0) error = %v", err)
	}
}

func FuzzPasswd_NeedsRehash(f *testing.F) {
	f.Add("$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2U")
	f.Add("$argon2id$v=19$m=4194304,t=2,p=1$$")
	f.Add("")
	f.Fuzz(func(t *testing.T, enc string) {
		_ = passwd.NeedsRehash(enc) // must not panic or allocate per the parameters
	})
}
