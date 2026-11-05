package notify

import (
	"crypto/ecdh"
	"encoding/base64"
	"testing"
)

// TestEncryptWebPush_RFC8291Vector: the encryption reproduces the worked
// example of RFC 8291 Appendix A byte for byte (the public test vector:
// keys, salt and secret are the RFC's, not Koro's).
func TestEncryptWebPush_RFC8291Vector(t *testing.T) {
	t.Parallel()
	b64 := func(s string) []byte {
		t.Helper()
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	asKey, err := ecdh.P256().NewPrivateKey(b64("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	ua, err := ecdh.P256().NewPublicKey(b64("BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"))
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.RawURLEncoding.EncodeToString(asKey.PublicKey().Bytes()); got !=
		"BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Fatalf("as_public = %s", got)
	}
	got, err := encryptWebPush([]byte("When I grow up, I want to be a watermelon"), ua, b64("BTBZMqHH6r4Tts7J_aSIgg"), asKey,
		b64("DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if s := base64.RawURLEncoding.EncodeToString(got); s != want {
		t.Fatalf("encrypted =\n%s\nwant\n%s", s, want)
	}
}
