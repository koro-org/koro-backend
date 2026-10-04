package events

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/problem"
)

// Ticket tokens (02 §11.7, R-003 §2.5.4):
//
//	token = base32(HMAC-SHA256(k_ticket[key_id], "koro-ticket-v1" ‖ registration_id ‖ version)[:16])
//
// 26 characters, the QR payload is "KORO1:<token>" (no name, e-mail or URL;
// BE-031 renders it). Only SHA-256(token) is stored (token_lookup), so the
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
