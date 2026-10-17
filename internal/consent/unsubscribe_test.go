package consent_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
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

// withLinks configures the unsubscribe links over d.
func withLinks(base string, d *crypto.Deriver) func(*consent.Deps) {
	return func(deps *consent.Deps) { deps.Unsubscribe = consent.UnsubscribeConfig{BaseURL: base, Deriver: d} }
}

// tokenOf mints userID's commercial e-mail link and returns its token.
func (e *env) tokenOf(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	link, err := e.svc.UnsubscribeURL(userID, consent.PurposeCommercialEmail)
	if err != nil {
		t.Fatalf("UnsubscribeURL: %v", err)
	}
	tok, ok := strings.CutPrefix(link, unsubBase+"/")
	if !ok || tok == "" || strings.ContainsAny(tok, "/?#=+") {
		t.Fatalf("link %q is not <base>/<base64url token>", link)
	}
	return tok
}

// handToken builds a token by the documented layout (unsubscribe.go) and
// signs it with d under purpose: it pins the layout and lets the tests
// forge tokens a real minting never produces.
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

// TestUnsubscribeToken_BoundToUserPurposeAndKey: a link verifies only as
// minted — for its user and purpose, under the consent.unsubscribe subkey
// of a key still in the keyring. Every other token is the same refusal.
func TestUnsubscribeToken_BoundToUserPurposeAndKey(t *testing.T) {
	t.Parallel()
	k1 := tokenKey("k1", 0x11)
	d := deriver(t, k1)
	e := newEnv(t, withLinks(unsubBase, d))
	ctx := t.Context()
	uid := uuid.NewV7()
	tok := e.tokenOf(t, uid)

	if l, err := e.svc.GetUnsubscribe(ctx, tok); err != nil || l.Purpose != consent.PurposeCommercialEmail {
		t.Fatalf("GetUnsubscribe(minted) = %+v, %v", l, err)
	}
	exp := e.clk.Now().Add(consent.DefaultUnsubscribeTTL).Truncate(time.Microsecond)
	if hand := handToken(t, d, "consent.unsubscribe", 1, 1, uid, exp, "k1"); hand != tok {
		t.Fatalf("minted token differs from the documented layout:\n got  %s\n want %s", tok, hand)
	}
	if other := e.tokenOf(t, uuid.NewV7()); other == tok {
		t.Fatal("two users got the same token")
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
	if fresh := rotated.tokenOf(t, uid); fresh == tok {
		t.Fatal("rotation did not change the signing key")
	}
	// A key dropped from the keyring retires its links.
	dropped := newEnv(t, withLinks(unsubBase, deriver(t, tokenKey("k2", 0x33))))
	dropped.refused(t, "dropped key", tok)

	// Only commercial e-mail has a link; a link needs a user.
	for _, p := range []consent.Purpose{consent.PurposeCommercialPush, consent.PurposeCommercialSMS, consent.PurposeDiscoveryProfile} {
		if _, err := e.svc.UnsubscribeURL(uid, p); err == nil {
			t.Errorf("UnsubscribeURL(%s) minted a link", p)
		}
	}
	if _, err := e.svc.UnsubscribeURL(uuid.Nil(), consent.PurposeCommercialEmail); err == nil {
		t.Error("UnsubscribeURL(nil user) minted a link")
	}
	if len(e.out.Events()) != 0 || len(e.audit.of(string(authz.ConsentRecordCreate))) != 0 {
		t.Fatal("verifying links wrote something")
	}
}

// TestUnsubscribe_ExpiredIsIndistinguishable: a link expires after the TTL
// and is then refused exactly like a forged one; until then it works.
func TestUnsubscribe_ExpiredIsIndistinguishable(t *testing.T) {
	t.Parallel()
	d := deriver(t, tokenKey("k1", 0x11))
	e := newEnv(t, func(deps *consent.Deps) {
		deps.Unsubscribe = consent.UnsubscribeConfig{BaseURL: unsubBase, Deriver: d, TTL: 48 * time.Hour}
	})
	uid := uuid.NewV7()
	e.record(t, grant(uid, e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01"), consent.ChannelWeb))
	tok := e.tokenOf(t, uid)

	e.clk.Advance(48*time.Hour - time.Second)
	if _, err := e.svc.GetUnsubscribe(t.Context(), tok); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	e.clk.Advance(time.Second)
	e.refused(t, "expired", tok)
	if ok, _ := e.svc.HasActive(t.Context(), uid, consent.PurposeCommercialEmail, nil); !ok {
		t.Fatal("an expired link withdrew the consent")
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
// e-mail); with keys, links minted earlier still verify; without keys
// every link is refused. The dependencies are validated at start.
func TestUnsubscribeURL_DisabledWithoutBaseURL(t *testing.T) {
	t.Parallel()
	d := deriver(t, tokenKey("k1", 0x11))
	on := newEnv(t, withLinks(unsubBase, d))
	if on.mod.Links == nil {
		t.Fatal("Module.Links is nil with a base URL")
	}
	uid := uuid.NewV7()
	tok := on.tokenOf(t, uid)

	off := newEnv(t, withLinks("", d))
	if off.mod.Links != nil {
		t.Fatal("Module.Links is set without a base URL")
	}
	if _, err := off.svc.UnsubscribeURL(uid, consent.PurposeCommercialEmail); !errors.Is(err, consent.ErrUnsubscribeUnavailable) {
		t.Fatalf("UnsubscribeURL without a base URL = %v", err)
	}
	if _, err := off.svc.GetUnsubscribe(t.Context(), tok); err != nil {
		t.Fatalf("a link minted earlier, base URL removed since: %v", err)
	}
	noKeys := newEnv(t)
	if noKeys.mod.Links != nil {
		t.Fatal("Module.Links is set without configuration")
	}
	noKeys.refused(t, "no keys", tok)

	for name, cfg := range map[string]consent.UnsubscribeConfig{
		"base without keys":  {BaseURL: unsubBase},
		"http":               {BaseURL: "http://koro.example.test/u", Deriver: d},
		"query":              {BaseURL: unsubBase + "?a=1", Deriver: d},
		"fragment":           {BaseURL: unsubBase + "#x", Deriver: d},
		"relative":           {BaseURL: "/unsubscribe", Deriver: d},
		"negative lifetime":  {BaseURL: unsubBase, Deriver: d, TTL: -time.Hour},
		"negative, disabled": {TTL: -time.Second},
	} {
		deps := memDeps(off)
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
