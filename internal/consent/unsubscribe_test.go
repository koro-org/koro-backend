package consent_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/platform/token"
)

const unsubBase = "https://koro.example.test/api/v1/unsubscribe-links"

// tokenKey is a synthetic HMAC key of the tests (never a real secret).
func tokenKey(id string, fill byte) crypto.Key {
	return crypto.Key{ID: id, Secret: bytes.Repeat([]byte{fill}, crypto.MinMACKeySize)}
}

func deriver(t *testing.T, keys ...crypto.Key) *crypto.Deriver {
	t.Helper()
	d, err := crypto.NewDeriver(keys)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// withLinks configures the unsubscribe links over d (d verifies the signed
// links of the BE-062 format).
func withLinks(base string, d *crypto.Deriver) func(*consent.Deps) {
	return func(deps *consent.Deps) { deps.Unsubscribe = consent.UnsubscribeConfig{BaseURL: base, Deriver: d} }
}

// tokenOf mints userID's commercial e-mail link and returns its opaque
// token.
func (e *env) tokenOf(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	link, err := e.svc.UnsubscribeURL(t.Context(), userID, consent.PurposeCommercialEmail)
	if err != nil {
		t.Fatalf("UnsubscribeURL: %v", err)
	}
	tok, ok := strings.CutPrefix(link, unsubBase+"/")
	if !ok || !token.WellFormed(tok) {
		t.Fatalf("link %q is not <base>/<43 base64url characters>", link)
	}
	return tok
}

// signedOf is userID's signed commercial e-mail link of the BE-062 format,
// as the links already in the mailboxes were minted: key k1 of d, expiry
// exp.
func signedOf(t *testing.T, d *crypto.Deriver, userID uuid.UUID, exp time.Time) string {
	t.Helper()
	return handToken(t, d, "consent.unsubscribe", 1, 1, userID, exp, "k1")
}

// handToken builds a signed token of the BE-062 layout
// (unsubscribe_legacy.go) and signs it with d under purpose: it pins the
// layout and lets the tests forge tokens the old minting never produced.
func handToken(t *testing.T, d *crypto.Deriver, purpose string, version, tag byte, userID uuid.UUID, exp time.Time, kid string) string {
	t.Helper()
	head := []byte{version, tag}
	head = append(head, userID[:]...)
	head = binary.BigEndian.AppendUint64(head, uint64(exp.Unix()))
	head = append(head, byte(len(kid)))
	head = append(head, kid...)
	mac, err := d.DerivedToken(purpose, kid, head)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(append(head, mac...))
}

// refused asserts that both endpoints answer tok with exactly the shared
// sentinel: no field errors, no wrapped cause, nothing that tells one
// reason from another.
func (e *env) refused(t *testing.T, name, tok string) {
	t.Helper()
	if _, err := e.svc.GetUnsubscribe(t.Context(), tok); !isBareInvalidLink(err) {
		t.Errorf("%s: GetUnsubscribe error = %v, want exactly ErrInvalidUnsubscribeLink", name, err)
	}
	if err := e.svc.Unsubscribe(t.Context(), tok); !isBareInvalidLink(err) {
		t.Errorf("%s: Unsubscribe error = %v, want exactly ErrInvalidUnsubscribeLink", name, err)
	}
}

// isBareInvalidLink reports whether err is the ErrInvalidUnsubscribeLink
// sentinel itself: not a copy with field errors or a wrapped cause, which
// could tell one refusal reason from another.
func isBareInvalidLink(err error) bool {
	var pe *problem.Error
	return errors.As(err, &pe) && pe == consent.ErrInvalidUnsubscribeLink && len(pe.Fields()) == 0 && errors.Unwrap(pe) == nil
}

// TestUnsubscribeToken_BoundToUserPurposeAndKey: a signed link of the
// BE-062 format verifies only as minted — for its user and purpose, under
// the consent.unsubscribe subkey of a key still in the keyring. Every other
// token is the same refusal.
func TestUnsubscribeToken_BoundToUserPurposeAndKey(t *testing.T) {
	t.Parallel()
	k1 := tokenKey("k1", 0x11)
	d := deriver(t, k1)
	e := newEnv(t, withLinks(unsubBase, d))
	ctx := t.Context()
	uid := uuid.NewV7()
	tok := signedOf(t, d, uid, e.clk.Now().Add(consent.DefaultUnsubscribeTTL))

	if l, err := e.svc.GetUnsubscribe(ctx, tok); err != nil || l.Purpose != consent.PurposeCommercialEmail {
		t.Fatalf("GetUnsubscribe(signed) = %+v, %v", l, err)
	}
	if len(tok) < 80 || len(tok) > consent.MaxUnsubscribeToken {
		t.Fatalf("a signed token is %d characters, outside the 80…200 spelling", len(tok))
	}

	// Every single-bit change of the token is refused.
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		b := bytes.Clone(raw)
		b[i] ^= 0x01
		e.refused(t, "flipped byte", base64.RawURLEncoding.EncodeToString(b))
	}

	future := e.clk.Now().Add(time.Hour)
	for name, forged := range map[string]string{
		"other derive purpose": handToken(t, d, "identity.email_token", 1, 1, uid, future, "k1"),
		"other secret, same key ID": handToken(t, deriver(t, tokenKey("k1", 0x22)), "consent.unsubscribe", 1, 1, uid,
			future, "k1"),
		"unknown key ID":   handToken(t, deriver(t, tokenKey("zz", 0x11)), "consent.unsubscribe", 1, 1, uid, future, "zz"),
		"unknown purpose":  handToken(t, d, "consent.unsubscribe", 1, 9, uid, future, "k1"),
		"unknown version":  handToken(t, d, "consent.unsubscribe", 2, 1, uid, future, "k1"),
		"nil user":         handToken(t, d, "consent.unsubscribe", 1, 1, uuid.Nil(), future, "k1"),
		"empty":            "",
		"short":            "abc",
		"not base64url":    "!!!" + tok[3:],
		"padded":           tok + "=",
		"standard base64":  base64.StdEncoding.EncodeToString(raw),
		"truncated":        tok[:len(tok)-2],
		"extended":         tok + "AA",
		"too long":         strings.Repeat("A", consent.MaxUnsubscribeToken+1),
		"path traversal":   "../" + tok,
		"head without mac": base64.RawURLEncoding.EncodeToString(raw[:len(raw)-32]),
		"embedded newline": tok[:20] + "\n" + tok[20:],
		"embedded CRLF":    tok[:20] + "\r\n" + tok[20:],
	} {
		e.refused(t, name, forged)
	}

	// Rotation: a newer active key still accepts the older key's links.
	rotated := newEnv(t, withLinks(unsubBase, deriver(t, tokenKey("k2", 0x33), k1)))
	if _, err := rotated.svc.GetUnsubscribe(ctx, tok); err != nil {
		t.Fatalf("link of the previous key after rotation: %v", err)
	}
	// A key dropped from the keyring retires its links.
	dropped := newEnv(t, withLinks(unsubBase, deriver(t, tokenKey("k2", 0x33))))
	dropped.refused(t, "dropped key", tok)
	if len(e.out.Events()) != 0 || len(e.audit.of(string(authz.ConsentRecordCreate))) != 0 {
		t.Fatal("verifying links wrote something")
	}
}

// TestUnsubscribeLink_OpaqueRandomAndStoredAsDigest (BE-153, Q-36): a
// link is <base>/<43 base64url characters> of 256 random bits, new for
// every e-mail, carrying nothing about its recipient; the store keeps only
// the SHA-256 of the token with the recipient, the purpose and the expiry.
// A token that is not a stored one — any changed character, a well-spelt
// stranger — is the same refusal as a forged signed link.
func TestUnsubscribeLink_OpaqueRandomAndStoredAsDigest(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withLinks(unsubBase+"/", nil)) // a trailing slash is trimmed; no key needed
	ctx := t.Context()
	uid := uuid.NewV7()
	tok := e.tokenOf(t, uid)

	raw, err := base64.RawURLEncoding.Strict().DecodeString(tok)
	if err != nil || len(raw) != 32 || len(tok) != token.EncodedLen {
		t.Fatalf("token %q: %d bytes, %v; want 32 random bytes in 43 characters", tok, len(raw), err)
	}
	if strings.Contains(tok, base64.RawURLEncoding.EncodeToString(uid[:])) || bytes.Contains(raw, uid[:]) {
		t.Fatal("the token carries the user ID")
	}
	seen := map[string]bool{tok: true}
	for range 20 { // new per e-mail, also for the same recipient
		next := e.tokenOf(t, uid)
		if seen[next] {
			t.Fatalf("token %q minted twice", next)
		}
		seen[next] = true
	}
	stored, err := e.store.UnsubscribeLinkByHash(ctx, token.Hash(tok))
	if err != nil || stored.UserID != uid || stored.Purpose != consent.PurposeCommercialEmail ||
		!stored.CreatedAt.Equal(e.clk.Now()) || !stored.ExpiresAt.Equal(e.clk.Now().Add(consent.DefaultUnsubscribeTTL)) {
		t.Fatalf("stored link = %+v, %v", stored, err)
	}
	if bytes.Contains(stored.TokenHash, raw) || bytes.Contains(stored.TokenHash, []byte(tok)) {
		t.Fatal("the store holds the token")
	}
	if l, err := e.svc.GetUnsubscribe(ctx, tok); err != nil || l.Purpose != consent.PurposeCommercialEmail {
		t.Fatalf("GetUnsubscribe(opaque) = %+v, %v", l, err)
	}

	// Every character change, and any well-spelt token never minted.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for i := range tok {
		c := alphabet[(strings.IndexByte(alphabet, tok[i])+1)%len(alphabet)]
		e.refused(t, "changed character "+strconv.Itoa(i), tok[:i]+string(c)+tok[i+1:])
	}
	stranger, _, err := token.New()
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"never minted":     stranger,
		"upper-cased":      strings.ToUpper(tok),
		"embedded newline": tok[:20] + "\n" + tok[21:],
		"padded":           tok[:42] + "=",
	} {
		e.refused(t, name, bad)
	}

	// Only commercial e-mail has a link; a link needs a user; neither is
	// stored. No minting or verification writes the ledger.
	for _, p := range []consent.Purpose{consent.PurposeCommercialPush, consent.PurposeCommercialSMS, consent.PurposeDiscoveryProfile} {
		if _, err := e.svc.UnsubscribeURL(ctx, uid, p); !errors.Is(err, consent.ErrUnsubscribeUnavailable) {
			t.Errorf("UnsubscribeURL(%s) = %v, want ErrUnsubscribeUnavailable", p, err)
		}
	}
	if _, err := e.svc.UnsubscribeURL(ctx, uuid.Nil(), consent.PurposeCommercialEmail); !errors.Is(err, consent.ErrUnsubscribeUnavailable) {
		t.Errorf("UnsubscribeURL(nil user) = %v", err)
	}
	if len(e.out.Events()) != 0 || len(e.audit.of(string(authz.ConsentRecordCreate))) != 0 {
		t.Fatal("minting or verifying links wrote something")
	}
}

// TestUnsubscribeLink_StoreFailureIsNoVerdict: a failing link store is an
// error of its own — never the invalid-link answer (which would tell the
// recipient the link is dead) and never a minted link without its row.
func TestUnsubscribeLink_StoreFailureIsNoVerdict(t *testing.T) {
	t.Parallel()
	var links *failingLinks
	e := newEnv(t, withLinks(unsubBase, nil), func(d *consent.Deps) {
		links = &failingLinks{Store: d.Store}
		d.Store = links
	})
	tok := e.tokenOf(t, uuid.NewV7())
	links.fail = true
	if _, err := e.svc.GetUnsubscribe(t.Context(), tok); !errors.Is(err, errLinkStore) {
		t.Fatalf("GetUnsubscribe with the store down = %v, want the store's error", err)
	}
	if err := e.svc.Unsubscribe(t.Context(), tok); !errors.Is(err, errLinkStore) {
		t.Fatalf("Unsubscribe with the store down = %v, want the store's error", err)
	}
	if link, err := e.svc.UnsubscribeURL(t.Context(), uuid.NewV7(), consent.PurposeCommercialEmail); link != "" ||
		!errors.Is(err, errLinkStore) || errors.Is(err, consent.ErrUnsubscribeUnavailable) {
		t.Fatalf("UnsubscribeURL with the store down = %q, %v; want a transient error", link, err)
	}
}

var errLinkStore = errors.New("link store down")

// failingLinks is a Store whose unsubscribe links fail while fail is set.
type failingLinks struct {
	consent.Store
	fail bool
}

func (f *failingLinks) InsertUnsubscribeLink(ctx context.Context, l consent.UnsubscribeToken) error {
	if f.fail {
		return errLinkStore
	}
	return f.Store.InsertUnsubscribeLink(ctx, l)
}

func (f *failingLinks) UnsubscribeLinkByHash(ctx context.Context, hash []byte) (consent.UnsubscribeToken, error) {
	if f.fail {
		return consent.UnsubscribeToken{}, errLinkStore
	}
	return f.Store.UnsubscribeLinkByHash(ctx, hash)
}

// TestUnsubscribe_ExpiredIsIndistinguishable: a link expires after the TTL
// and is then refused exactly like a forged one; until then it works. The
// same holds for a signed link of the BE-062 format at its own expiry: the
// transition ends link by link, no switch needed.
func TestUnsubscribe_ExpiredIsIndistinguishable(t *testing.T) {
	t.Parallel()
	d := deriver(t, tokenKey("k1", 0x11))
	e := newEnv(t, func(deps *consent.Deps) {
		deps.Unsubscribe = consent.UnsubscribeConfig{BaseURL: unsubBase, Deriver: d, TTL: 48 * time.Hour}
	})
	uid := uuid.NewV7()
	e.record(t, grant(uid, e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01"), consent.ChannelWeb))
	tok := e.tokenOf(t, uid)
	signed := signedOf(t, d, uid, e.clk.Now().Add(48*time.Hour))

	e.clk.Advance(48*time.Hour - time.Second)
	for name, tok := range map[string]string{"opaque": tok, "signed": signed} {
		if _, err := e.svc.GetUnsubscribe(t.Context(), tok); err != nil {
			t.Fatalf("%s, one second before expiry: %v", name, err)
		}
	}
	e.clk.Advance(time.Second)
	e.refused(t, "expired", tok)
	e.refused(t, "expired signed", signed)
	if ok, _ := e.svc.HasActive(t.Context(), uid, consent.PurposeCommercialEmail, nil); !ok {
		t.Fatal("an expired link withdrew the consent")
	}
}

// TestUnsubscribe_SignedLinkStillWithdraws (BE-153 transition): a signed
// link of the BE-062 format already in a mailbox withdraws like an opaque
// one (same ledger row, audit, İYS refusal) until it expires; without the
// token keys it is refused (fail closed) while opaque links need none.
func TestUnsubscribe_SignedLinkStillWithdraws(t *testing.T) {
	t.Parallel()
	d := deriver(t, tokenKey("k1", 0x11))
	e := newEnv(t, withLinks(unsubBase, d))
	ctx := t.Context()
	uid := uuid.NewV7()
	e.record(t, grant(uid, e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01"), consent.ChannelWeb))
	signed := signedOf(t, d, uid, e.clk.Now().Add(consent.DefaultUnsubscribeTTL))
	if err := e.svc.Unsubscribe(ctx, signed); err != nil {
		t.Fatalf("Unsubscribe(signed) = %v", err)
	}
	c, found, err := e.store.Latest(ctx, uid, consent.PurposeCommercialEmail, nil)
	if err != nil || !found || c.Action != consent.Withdrawn || c.Channel != consent.ChannelEmailLink {
		t.Fatalf("latest after a signed link = %+v, %v, %v", c, found, err)
	}
	if evs := e.audit.of(string(authz.ConsentRecordCreate)); len(evs) == 0 || *evs[len(evs)-1].ActorUserID != uid {
		t.Fatalf("audit of the signed withdrawal = %+v", evs)
	}
	if _, err := e.store.SubmissionOf(ctx, c.ID); err != nil {
		t.Fatalf("İYS submission of the signed withdrawal: %v", err)
	}

	keyless := newEnv(t, withLinks(unsubBase, nil))
	keyless.refused(t, "signed without keys", signed)
	if _, err := keyless.svc.GetUnsubscribe(ctx, keyless.tokenOf(t, uid)); err != nil {
		t.Fatalf("opaque link without keys: %v", err)
	}
}

// TestUnsubscribe_UsedLinkStaysValidUntilExpiry: a link is not single use
// (the semantics of BE-062): its second click is a no-op with the same
// answer, and after the user granted again the same older mail's link
// still refuses — the legal opt-out never depends on which mail is used.
func TestUnsubscribe_UsedLinkStaysValidUntilExpiry(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withLinks(unsubBase, nil))
	ctx := t.Context()
	uid := uuid.NewV7()
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	e.record(t, grant(uid, text, consent.ChannelWeb))
	tok := e.tokenOf(t, uid)
	for range 2 {
		if err := e.svc.Unsubscribe(ctx, tok); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Second)
	}
	if rows, _ := e.store.AllOf(ctx, uid); len(rows) != 2 {
		t.Fatalf("rows after two clicks = %d, want the grant and one withdrawal", len(rows))
	}
	e.record(t, grant(uid, text, consent.ChannelMobile))
	if err := e.svc.Unsubscribe(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.HasActive(ctx, uid, consent.PurposeCommercialEmail, nil); ok {
		t.Fatal("the used link did not withdraw the new grant")
	}
	if l, err := e.svc.GetUnsubscribe(ctx, tok); err != nil || l.Purpose != consent.PurposeCommercialEmail {
		t.Fatalf("GetUnsubscribe(used) = %+v, %v", l, err)
	}
}

// TestUnsubscribe_ErasedAccountLinkRefused (KVKK): erasing the account
// deletes its links in the erasure's transaction; they are then refused
// like unknown ones, and a second erasure deletes nothing. Another user's
// links stay.
func TestUnsubscribe_ErasedAccountLinkRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withLinks(unsubBase, nil))
	ctx := t.Context()
	gone, stays := uuid.NewV7(), uuid.NewV7()
	e.record(t, grant(gone, e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01"), consent.ChannelWeb))
	first, second, other := e.tokenOf(t, gone), e.tokenOf(t, gone), e.tokenOf(t, stays)

	res, err := e.mod.Privacy.Erase(ctx, gone)
	if err != nil || res.Deleted != 2 {
		t.Fatalf("Erase = %+v, %v; want the two links deleted", res, err)
	}
	e.refused(t, "erased, first link", first)
	e.refused(t, "erased, second link", second)
	if _, err := e.svc.GetUnsubscribe(ctx, other); err != nil {
		t.Fatalf("another user's link after the erasure: %v", err)
	}
	if again, err := e.mod.Privacy.Erase(ctx, gone); err != nil || again.Deleted != 0 {
		t.Fatalf("second Erase = %+v, %v", again, err)
	}
	sec, err := e.mod.Privacy.Export(ctx, stays)
	if err != nil || len(sec.Items) != 0 {
		t.Fatalf("Export of a user with a link only = %+v, %v; links are not exported", sec, err)
	}
}

// TestUnsubscribeCleanup_DeletesExpiredLinks: consent.unsubscribe_links_cleanup
// deletes the links that expired more than the policy's period ago (7 days
// by default), nothing younger; an error reading the period fails the run.
func TestUnsubscribeCleanup_DeletesExpiredLinks(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(deps *consent.Deps) {
		deps.Unsubscribe = consent.UnsubscribeConfig{BaseURL: unsubBase, TTL: 24 * time.Hour}
	})
	ctx := t.Context()
	uid := uuid.NewV7()
	old := e.tokenOf(t, uid)
	e.clk.Advance(2 * 24 * time.Hour)
	recent := e.tokenOf(t, uid) // expires a day from now
	stored := func(tok string) bool {
		_, err := e.store.UnsubscribeLinkByHash(ctx, token.Hash(tok))
		return err == nil
	}
	job := e.mod.UnsubscribeCleanupJob(nil)
	if job.Name != consent.UnsubscribeCleanupJobName || job.Cron != "50 3 * * *" || job.Queue != "low" || job.Validate() != nil {
		t.Fatalf("job = %s %s %s, %v", job.Name, job.Cron, job.Queue, job.Validate())
	}
	if err := job.Handle(ctx); err != nil || !stored(old) || !stored(recent) {
		t.Fatalf("a day after the expiry: %v (old kept %v)", err, stored(old))
	}
	e.clk.Advance(6*24*time.Hour + time.Second) // old expired 7 days and a second ago
	if err := job.Handle(ctx); err != nil || stored(old) || !stored(recent) {
		t.Fatalf("7 days after the expiry: %v (old kept %v, recent kept %v)", err, stored(old), stored(recent))
	}
	byPolicy := e.mod.UnsubscribeCleanupJob(func(context.Context) (time.Duration, error) { return time.Hour, nil })
	e.clk.Advance(2 * time.Hour) // recent expired days ago: an hour's period lets it go
	if err := byPolicy.Handle(ctx); err != nil || stored(recent) {
		t.Fatalf("the policy's period: %v (recent kept %v)", err, stored(recent))
	}
	broken := e.mod.UnsubscribeCleanupJob(func(context.Context) (time.Duration, error) { return 0, errLinkStore })
	if err := broken.Handle(ctx); !errors.Is(err, errLinkStore) {
		t.Fatalf("a failing period = %v", err)
	}
}

// TestUnsubscribe_WithdrawsAndQueuesIYSRet: one click withdraws the
// consent in force through the email_link channel — the subject's own act
// in the ledger and the audit log, with the request's IP and User-Agent as
// proof — and queues the İYS refusal (RET) through the consent.recorded
// outbox event (BE-012/BE-055 path).
func TestUnsubscribe_WithdrawsAndQueuesIYSRet(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withLinks(unsubBase, deriver(t, tokenKey("k1", 0x11))))
	uid := uuid.NewV7()
	e.record(t, grant(uid, e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01"), consent.ChannelWeb))
	e.record(t, grant(uid, e.consentText(t, consent.PurposeCommercialPush, "2026.10.01"), consent.ChannelMobile))
	auditBefore, eventsBefore := len(e.audit.of(string(authz.ConsentRecordCreate))), len(e.out.Events())

	ctx := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), "203.0.113.9"), "Mailbox-OneClick/1.0")
	if err := e.svc.Unsubscribe(ctx, e.tokenOf(t, uid)); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if ok, _ := e.svc.HasActive(t.Context(), uid, consent.PurposeCommercialEmail, nil); ok {
		t.Fatal("commercial_email still in force")
	}
	if ok, _ := e.svc.HasActive(t.Context(), uid, consent.PurposeCommercialPush, nil); !ok {
		t.Fatal("the e-mail link withdrew the push consent too")
	}
	c, found, err := e.store.Latest(t.Context(), uid, consent.PurposeCommercialEmail, nil)
	switch {
	case err != nil || !found:
		t.Fatalf("Latest = %v, %v", found, err)
	case c.Action != consent.Withdrawn || c.Channel != consent.ChannelEmailLink || c.NoticeVersionID != nil ||
		c.ScopeID != nil || c.IP != "203.0.113.9" || c.UserAgent != "Mailbox-OneClick/1.0":
		t.Fatalf("withdrawal row = %+v", c)
	}

	evs := e.audit.of(string(authz.ConsentRecordCreate))
	if len(evs) != auditBefore+1 {
		t.Fatalf("audit rows = %d, want %d", len(evs), auditBefore+1)
	}
	ev := evs[len(evs)-1]
	if ev.ActorUserID == nil || *ev.ActorUserID != uid || ev.SystemJob != "" || ev.ResourceID == nil || *ev.ResourceID != c.ID ||
		ev.Metadata["channel"] != "email_link" || ev.Metadata["consent_action"] != "withdrawn" {
		t.Fatalf("audit = %+v", ev)
	}

	evts := e.out.Events()
	if len(evts) != eventsBefore+1 || evts[len(evts)-1].Topic != domainevent.TopicConsentRecorded {
		t.Fatalf("outbox events %d (+%d)", len(evts), len(evts)-eventsBefore)
	}
	for _, err := range e.deliverAll(t) {
		if err != nil {
			t.Fatal(err)
		}
	}
	var ret []consent.IYSRecord
	for _, r := range e.iys.Sent() {
		if r.ConsentID == c.ID {
			ret = append(ret, r)
		}
	}
	if len(ret) != 1 || ret[0].Action != consent.Withdrawn || ret[0].Source != consent.ChannelEmailLink ||
		ret[0].Channel != consent.IYSEmail || ret[0].UserID != uid {
		t.Fatalf("İYS records of the withdrawal = %+v (want one RET, source email_link → HS_EPOSTA)", ret)
	}
	if sub, err := e.store.SubmissionOf(t.Context(), c.ID); err != nil || sub.Status != consent.SubmissionSent {
		t.Fatalf("submission = %+v, %v", sub, err)
	}
}

// TestUnsubscribe_IdempotentWithoutStateLeak: a repeated click, a user
// who never consented and a user who already withdrew all get the same
// answer, and none of them writes a row, an event or an audit entry.
func TestUnsubscribe_IdempotentWithoutStateLeak(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withLinks(unsubBase, deriver(t, tokenKey("k1", 0x11))))
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	granted, never, withdrew := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	e.record(t, grant(granted, text, consent.ChannelWeb))
	e.record(t, grant(withdrew, text, consent.ChannelWeb))
	e.record(t, withdraw(withdrew, consent.PurposeCommercialEmail))

	tok := e.tokenOf(t, granted)
	if err := e.svc.Unsubscribe(ctx, tok); err != nil {
		t.Fatal(err)
	}
	type snapshot struct{ rows, events, audits int }
	snap := func() snapshot {
		n := 0
		for _, u := range []uuid.UUID{granted, never, withdrew} {
			rows, err := e.store.AllOf(ctx, u)
			if err != nil {
				t.Fatal(err)
			}
			n += len(rows)
		}
		return snapshot{n, len(e.out.Events()), len(e.audit.of(string(authz.ConsentRecordCreate)))}
	}
	before := snap()
	for name, tok := range map[string]string{
		"second click": tok, "never consented": e.tokenOf(t, never), "already withdrawn": e.tokenOf(t, withdrew),
	} {
		l, err := e.svc.GetUnsubscribe(ctx, tok)
		if err != nil || l != (consent.UnsubscribeLink{Purpose: consent.PurposeCommercialEmail}) {
			t.Errorf("%s: GetUnsubscribe = %+v, %v", name, l, err)
		}
		if err := e.svc.Unsubscribe(ctx, tok); err != nil {
			t.Errorf("%s: Unsubscribe = %v, want nil (same answer)", name, err)
		}
	}
	if after := snap(); after != before {
		t.Fatalf("no-op clicks wrote: before %+v, after %+v", before, after)
	}
	if rows, _ := e.store.AllOf(ctx, granted); len(rows) != 2 {
		t.Fatalf("granted user has %d rows, want the grant and one withdrawal", len(rows))
	}
}

// A temporarily disabled purpose must not revive a grant that the user
// withdrew through an old e-mail while the purpose was disabled.
func TestUnsubscribe_InactivePurposeStillRecordsWithdrawal(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withLinks(unsubBase, deriver(t, tokenKey("k1", 0x11))))
	ctx, uid, purpose := t.Context(), uuid.NewV7(), consent.PurposeCommercialEmail
	e.record(t, grant(uid, e.consentText(t, purpose, "2026.10.01"), consent.ChannelWeb))
	tok := e.tokenOf(t, uid)
	if _, err := e.svc.UpdatePurpose(ctx, actor(t, "dpo"), purpose, consent.PurposePatch{Active: ptr(false)}, proof); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Unsubscribe(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdatePurpose(ctx, actor(t, "dpo"), purpose, consent.PurposePatch{Active: ptr(true)}, proof); err != nil {
		t.Fatal(err)
	}
	if active, err := e.svc.HasActive(ctx, uid, purpose, nil); err != nil || active {
		t.Fatalf("HasActive after re-enabling purpose = %v, %v", active, err)
	}
	c, found, err := e.store.Latest(ctx, uid, purpose, nil)
	if err != nil || !found || c.Action != consent.Withdrawn || c.Channel != consent.ChannelEmailLink {
		t.Fatalf("latest = %+v, %v, %v", c, found, err)
	}
	if sub, err := e.store.SubmissionOf(ctx, c.ID); err != nil || sub.Status != consent.SubmissionPending {
		t.Fatalf("withdrawal submission = %+v, %v", sub, err)
	}
}

// TestGetUnsubscribeLink_MinimalData: the confirmation page gets the
// purpose and nothing else (no user, no address, no consent state), and
// reading it changes nothing (mail scanners open links).
func TestGetUnsubscribeLink_MinimalData(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withLinks(unsubBase, deriver(t, tokenKey("k1", 0x11))))
	uid := uuid.NewV7()
	e.record(t, grant(uid, e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01"), consent.ChannelWeb))
	before := len(e.out.Events())
	tok := e.tokenOf(t, uid)
	for range 3 {
		l, err := e.svc.GetUnsubscribe(t.Context(), tok)
		if err != nil || l.Purpose != consent.PurposeCommercialEmail {
			t.Fatalf("GetUnsubscribe = %+v, %v", l, err)
		}
	}
	if ok, _ := e.svc.HasActive(t.Context(), uid, consent.PurposeCommercialEmail, nil); !ok || len(e.out.Events()) != before {
		t.Fatal("GET changed the consent")
	}
	b, err := json.Marshal(consent.UnsubscribeLinkDTO{PurposeCode: string(consent.PurposeCommercialEmail)})
	if err != nil || string(b) != `{"purpose_code":"commercial_email"}` {
		t.Fatalf("UnsubscribeLinkDTO = %s, %v", b, err)
	}
}

// TestUnsubscribeURL_DisabledWithoutBaseURL: without a base URL no link is
// minted (Module.Links is nil, outreach keeps commercial dispatches off
// e-mail); the links minted earlier still work; without keys the signed
// links of the BE-062 format are refused. The dependencies are validated
// at start.
func TestUnsubscribeURL_DisabledWithoutBaseURL(t *testing.T) {
	t.Parallel()
	d := deriver(t, tokenKey("k1", 0x11))
	on := newEnv(t, withLinks(unsubBase, d))
	if on.mod.Links == nil {
		t.Fatal("Module.Links is nil with a base URL")
	}
	uid := uuid.NewV7()
	tok := on.tokenOf(t, uid)
	signed := signedOf(t, d, uid, on.clk.Now().Add(time.Hour))

	deps := memDeps(on) // the same store: the base URL was removed since
	deps.Unsubscribe = consent.UnsubscribeConfig{Deriver: d}
	off, err := consent.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if off.Links != nil {
		t.Fatal("Module.Links is set without a base URL")
	}
	if _, err := off.Service.UnsubscribeURL(t.Context(), uid, consent.PurposeCommercialEmail); !errors.Is(err, consent.ErrUnsubscribeUnavailable) {
		t.Fatalf("UnsubscribeURL without a base URL = %v", err)
	}
	for name, tok := range map[string]string{"opaque": tok, "signed": signed} {
		if _, err := off.Service.GetUnsubscribe(t.Context(), tok); err != nil {
			t.Fatalf("a %s link minted earlier, base URL removed since: %v", name, err)
		}
	}
	noKeys := newEnv(t)
	if noKeys.mod.Links != nil {
		t.Fatal("Module.Links is set without configuration")
	}
	noKeys.refused(t, "no keys", signed)
	noKeys.refused(t, "another store", tok)

	keyless := memDeps(on)
	keyless.Unsubscribe = consent.UnsubscribeConfig{BaseURL: unsubBase}
	if _, err := consent.New(keyless); err != nil {
		t.Fatalf("a base URL without keys (opaque links only) = %v", err)
	}
	for name, cfg := range map[string]consent.UnsubscribeConfig{
		"http":               {BaseURL: "http://koro.example.test/u", Deriver: d},
		"query":              {BaseURL: unsubBase + "?a=1", Deriver: d},
		"fragment":           {BaseURL: unsubBase + "#x", Deriver: d},
		"relative":           {BaseURL: "/unsubscribe", Deriver: d},
		"negative lifetime":  {BaseURL: unsubBase, Deriver: d, TTL: -time.Hour},
		"negative, disabled": {TTL: -time.Second},
	} {
		deps := memDeps(on)
		deps.Unsubscribe = cfg
		if _, err := consent.New(deps); err == nil || !strings.Contains(err.Error(), "Unsubscribe") {
			t.Errorf("%s: New error = %v", name, err)
		}
	}
}

// memDeps returns complete in-memory dependencies over e's store and fakes.
func memDeps(e *env) consent.Deps {
	return consent.Deps{
		Store: e.store, Tx: db.NopTx{}, Outbox: e.out, Audit: e.audit, Clock: e.clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: db.NopTx{}, Store: idempotency.NewMemStore(), Clock: e.clk, Log: obs.Discard()}),
		Reauth:      fakeReauth{}, IYS: e.iys,
	}
}

// TestUnsubscribeEndpoints_SpellingAndRateLimit pins the HTTP contract of
// the two public operations (BE-153): the token parameter's pattern admits
// exactly the two spellings — an opaque token (43) and a signed one of the
// BE-062 format (80 to 200) — and both formats answer through it; another
// spelling is 422 validation_failed before any lookup. The guessing and
// counting defence is the token's 256 random bits (or the MAC) plus the
// per-IP read_default/write_default limits under global_ip (04 §1.9; not
// token_confirm: mailbox providers post from shared addresses).
func TestUnsubscribeEndpoints_SpellingAndRateLimit(t *testing.T) {
	t.Parallel()
	d := deriver(t, tokenKey("k1", 0x11))
	e := newEnv(t, withLinks(unsubBase, d))
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(authztest.Personas()...)})
	if err != nil {
		t.Fatal(err)
	}
	consent.Register(app.V1, e.svc)
	p := app.API.OpenAPI().Paths["/api/v1/unsubscribe-links/{token}"]
	if p == nil || p.Get == nil || p.Post == nil {
		t.Fatal("unsubscribe-links operations not registered")
	}
	re := regexp.MustCompile("^(?:[A-Za-z0-9_-]{43}|[A-Za-z0-9_-]{80,200})$")
	for _, o := range []struct {
		op   *huma.Operation
		rate string
	}{{p.Get, "read_default"}, {p.Post, "write_default"}} {
		if got := fmt.Sprint(o.op.Extensions["x-koro-rate-limit"]); got != o.rate {
			t.Errorf("%s rate class = %s, want %s", o.op.OperationID, got, o.rate)
		}
		if len(o.op.Parameters) != 1 || o.op.Parameters[0].Schema == nil || o.op.Parameters[0].Schema.Pattern != re.String() {
			t.Fatalf("%s token parameter = %+v", o.op.OperationID, o.op.Parameters)
		}
	}

	uid := uuid.NewV7()
	opaque, signed := e.tokenOf(t, uid), signedOf(t, d, uid, e.clk.Now().Add(time.Hour))
	do := func(method, tok string) (int, string) {
		req := httptest.NewRequestWithContext(t.Context(), method, "/api/v1/unsubscribe-links/"+tok, strings.NewReader("List-Unsubscribe=One-Click"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	for name, tok := range map[string]string{"opaque": opaque, "signed": signed} {
		if !re.MatchString(tok) {
			t.Fatalf("the %s token %q does not match the pattern", name, tok)
		}
		if code, body := do(http.MethodGet, tok); code != http.StatusOK || !strings.Contains(body, `"purpose_code":"commercial_email"`) {
			t.Errorf("GET %s = %d %s", name, code, body)
		}
		if code, body := do(http.MethodPost, tok); code != http.StatusNoContent {
			t.Errorf("POST %s = %d %s", name, code, body)
		}
	}
	for name, tok := range map[string]string{
		"44 characters": opaque + "A", "79 characters": strings.Repeat("A", 79), "201 characters": strings.Repeat("A", 201),
		"a dot": opaque[:42] + ".",
	} {
		code, body := do(http.MethodGet, tok)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"code":"validation_failed"`) || strings.Contains(body, tok) {
			t.Errorf("GET %s = %d %s; want 422 validation_failed without the token", name, code, body)
		}
	}
	stranger, _, err := token.New()
	if err != nil {
		t.Fatal(err)
	}
	if code, body := do(http.MethodPost, stranger); code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, `"code":"consent.invalid_unsubscribe_link"`) {
		t.Errorf("POST a well-spelt stranger = %d %s", code, body)
	}
}
