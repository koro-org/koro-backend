package events

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"koro/internal/platform/crypto"
	"koro/internal/platform/problem"
)

// Ticket tokens (02 §11.7, R-003 §2.5.4):
//
//	token = base32(HMAC-SHA256(k_ticket[key_id], "koro-ticket-v1" ‖ registration_id ‖ version)[:16])
//
// 26 characters, the QR payload is "KORO1:<token>" (no name, e-mail or URL;
// BE-031 renders it, QRRenderer). Only SHA-256(token) is stored (token_lookup), so the
// token is re-derived for a resend and a database leak yields no ticket.
// A re-issue or a cancellation moves to version + 1 or revokes the row.
// The manual code is 40 random bits in Crockford base32 (8 characters),
// unique per event.
const (
	// TicketTokenPurpose is the derivation purpose of ticket tokens.
	TicketTokenPurpose = "events.ticket"
	// TicketQRPrefix prefixes the token in the QR payload.
	TicketQRPrefix = "KORO1:"

	ticketDomain     = "koro-ticket-v1"
	ticketTokenBytes = 16
	ticketTokenLen   = 26 // base32 of 16 bytes without padding
	manualCodeLen    = 8
	ticketAttempts   = 5 // manual code draws before giving up (a collision is 1 in 2^40 per ticket)
)

var (
	ticketEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)
	// crockford is Crockford's base32 alphabet (no I, L, O, U; the CHECK
	// registration_tickets_manual_code_chk).
	crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)
)

// TicketToken derives the token of version of a registration under keyID.
// BE-031 (QR e-mail, resend) and the check-in lookup use it.
func TicketToken(keys TicketKeys, keyID string, registrationID uuid.UUID, version int) (string, error) {
	if keys == nil {
		return "", problem.ErrUnavailable
	}
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], uint64(version)) //nolint:gosec // version > 0 (CHECK)
	mac, err := keys.DerivedToken(TicketTokenPurpose, keyID, []byte(ticketDomain), registrationID[:], v[:])
	if err != nil {
		return "", fmt.Errorf("events: derive ticket token: %w", err)
	}
	return ticketEncoding.EncodeToString(mac[:ticketTokenBytes]), nil
}

// TicketLookup is token_lookup: the SHA-256 of the token text.
func TicketLookup(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// newManualCode draws a manual code: 40 random bits, Crockford base32.
func newManualCode() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return crockford.EncodeToString(b)
}

// issueTicket writes the next active ticket of a confirmed registration in
// the caller's transaction and returns it (the token itself is never
// stored or returned; BE-031 re-derives it).
func (s *service) issueTicket(ctx context.Context, r Registration, now time.Time) (Ticket, error) {
	if s.d.Tickets == nil {
		return Ticket{}, problem.ErrUnavailable
	}
	last, err := s.d.Store.MaxTicketVersion(ctx, r.OrgID, r.ID)
	if err != nil {
		return Ticket{}, err
	}
	keyID := s.d.Tickets.ActiveKeyID()
	version := last + 1
	token, err := TicketToken(s.d.Tickets, keyID, r.ID, version)
	if err != nil {
		return Ticket{}, err
	}
	t := Ticket{
		RegistrationID: r.ID, EventID: r.EventID, Version: version, KeyID: keyID, TokenLookup: TicketLookup(token),
		Status: TicketActive, IssuedAt: now,
	}
	for range ticketAttempts {
		t.ID, t.ManualCode = uuid.NewV7(), newManualCode()
		stored, ok, err := s.d.Store.InsertTicket(ctx, r.OrgID, t)
		if err != nil {
			return Ticket{}, err
		}
		if ok {
			return stored, nil
		}
	}
	return Ticket{}, fmt.Errorf("events: no free manual code after %d draws", ticketAttempts)
}

// TicketPayload is the QR content of a token: "KORO1:<token>" (R-003
// §2.5.4; QR alphanumeric mode, no PII, no URL).
func TicketPayload(token string) string { return TicketQRPrefix + token }

// Ticket verification errors (BE-031). Neither carries the token.
var (
	// errTicketMAC: the presented token is not the one derived for the
	// ticket row it was found under (a forged or tampered row).
	errTicketMAC = errors.New("events: ticket token does not verify")
	// errTicketKey: the ticket's key_id is no longer in CRYPTO_TOKEN_KEYS (a
	// retired key; the ticket must be reissued).
	errTicketKey = errors.New("events: ticket key is retired")
)

// parseQRPayload is the format check of a scanned QR payload: "KORO1:"
// followed by exactly 26 characters of the RFC 4648 base32 alphabet that
// decode to 16 bytes canonically (the last character carries 2 zero bits).
// It returns the token; it does not authenticate it (verifyTicket does,
// after the lookup). Unexported on purpose (05 §14.2: no exported decoder
// without verification).
func parseQRPayload(s string) (string, bool) {
	tok, ok := strings.CutPrefix(s, TicketQRPrefix)
	if !ok || len(tok) != ticketTokenLen {
		return "", false
	}
	for i := range len(tok) {
		c := tok[i]
		if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			return "", false
		}
	}
	raw, err := ticketEncoding.DecodeString(tok)
	if err != nil || len(raw) != ticketTokenBytes || ticketEncoding.EncodeToString(raw) != tok {
		return "", false
	}
	return tok, true
}

// normalizeManualCode reads a manual code as typed at the door: case and
// the separators of the printed form ("7KQ2-M9TX", spaces) are ignored and
// Crockford's look-alikes are mapped (I, L → 1; O → 0). It reports false
// for anything that is not 8 Crockford characters.
func normalizeManualCode(s string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case '-', ' ':
			continue
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if b.Len() >= manualCodeLen {
			return "", false
		}
		b.WriteRune(r)
	}
	code := b.String()
	if !manualCodeRe.MatchString(code) {
		return "", false
	}
	return code, true
}

// displayManualCode is the printed form of a manual code ("7KQ2-M9TX").
func displayManualCode(code string) string {
	if len(code) != manualCodeLen {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// ticketToken re-derives the token of t (its own key_id and version):
// errTicketKey when the key is retired.
func (s *service) ticketToken(t Ticket) (string, error) {
	token, err := TicketToken(s.d.Tickets, t.KeyID, t.RegistrationID, t.Version)
	if errors.Is(err, crypto.ErrUnknownKey) {
		return "", errTicketKey
	}
	return token, err
}

// verifyTicket authenticates a presented token against the ticket row it
// was found under: the token derived for (key_id, registration_id,
// version) must equal it, compared in constant time (05 §14.2). A row
// planted in the database without the key does not verify (errTicketMAC);
// a retired key is errTicketKey.
func (s *service) verifyTicket(t Ticket, presented string) error {
	want, err := s.ticketToken(t)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(presented)) != 1 {
		return errTicketMAC
	}
	return nil
}
