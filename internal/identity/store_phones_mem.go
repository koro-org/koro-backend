package identity

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/sms"
)

// The phone and SMS code rows of the in-memory store (BE-172): the same
// rules as 000177 (user_phones_phone_uq, the CHECKs, the users FK).

// Phone implements Store.
func (m *MemStore) Phone(_ context.Context, userID uuid.UUID) (Phone, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.phones[userID]
	if !ok {
		return Phone{}, false, nil
	}
	p.SMSMFAEnabledAt = cloneTime(p.SMSMFAEnabledAt)
	return p, true, nil
}

// PhoneOwner implements Store.
func (m *MemStore) PhoneOwner(_ context.Context, number string) (uuid.UUID, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, p := range m.phones {
		if p.Number == number {
			return id, true, nil
		}
	}
	return uuid.Nil(), false, nil
}

// PutPhone implements Store.
func (m *MemStore) PutPhone(_ context.Context, p Phone) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[p.UserID]; !ok {
		return fmt.Errorf("identity memstore: phone of unknown user: %w", ErrNotFound)
	}
	if !sms.ValidNumber(p.Number) || p.VerifiedAt.IsZero() {
		return fmt.Errorf("%w: user_phones", errMemCheck)
	}
	for id, o := range m.phones {
		if o.Number == p.Number && id != p.UserID {
			return ErrPhoneTaken
		}
	}
	if cur, ok := m.phones[p.UserID]; ok {
		p.CreatedAt = cur.CreatedAt
	} else {
		p.CreatedAt = m.stamp()
	}
	p.SMSMFAEnabledAt = cloneTime(p.SMSMFAEnabledAt)
	m.phones[p.UserID] = p
	return nil
}

// SetSMSMFA implements Store.
func (m *MemStore) SetSMSMFA(_ context.Context, userID uuid.UUID, at *time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.phones[userID]
	if !ok {
		return false, nil
	}
	p.SMSMFAEnabledAt = cloneTime(at)
	m.phones[userID] = p
	return true, nil
}

// DeletePhone implements Store.
func (m *MemStore) DeletePhone(_ context.Context, userID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.phones[userID]; !ok {
		return 0, nil
	}
	delete(m.phones, userID)
	return 1, nil
}

// InsertSMSCode implements Store.
func (m *MemStore) InsertSMSCode(_ context.Context, c SMSCode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[c.UserID]; !ok {
		return fmt.Errorf("identity memstore: SMS code of unknown user: %w", ErrNotFound)
	}
	switch {
	case c.Purpose != SMSPhoneVerify && c.Purpose != SMSLogin && c.Purpose != SMSPasswordReset,
		!sms.ValidNumber(c.Number), c.KeyID == "", len(c.CodeHash) != 32, c.Attempts < 0 || c.Attempts > maxSMSAttemptsStored:
		return fmt.Errorf("%w: sms_codes", errMemCheck)
	}
	if _, dup := m.smsCodes[c.ID]; dup {
		return fmt.Errorf("identity memstore: duplicate SMS code %s", c.ID)
	}
	if !c.ExpiresAt.After(c.CreatedAt) {
		return fmt.Errorf("%w: sms_codes_expiry_chk", errMemCheck)
	}
	m.smsCodes[c.ID] = c.clone()
	return nil
}

// SMSCode implements Store.
func (m *MemStore) SMSCode(_ context.Context, id uuid.UUID) (SMSCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.smsCodes[id]
	if !ok {
		return SMSCode{}, ErrNotFound
	}
	return c.clone(), nil
}

// OpenSMSCode implements Store: the newest open code (UUIDv7 order, as the
// GORM store's ORDER BY id DESC).
func (m *MemStore) OpenSMSCode(_ context.Context, userID uuid.UUID, purpose SMSPurpose) (SMSCode, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best SMSCode
	found := false
	for _, c := range m.smsCodes {
		if c.UserID != userID || c.Purpose != purpose || c.ConsumedAt != nil {
			continue
		}
		if !found || c.ID.Compare(best.ID) > 0 {
			best, found = c, true
		}
	}
	return best.clone(), found, nil
}

// CloseOpenSMSCodes implements Store.
func (m *MemStore) CloseOpenSMSCodes(_ context.Context, userID uuid.UUID, purpose SMSPurpose, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.smsCodes {
		if c.UserID == userID && c.Purpose == purpose && c.ConsumedAt == nil {
			c.ConsumedAt = &at
			m.smsCodes[id] = c
		}
	}
	return nil
}

// AddSMSCodeAttempt implements Store.
func (m *MemStore) AddSMSCodeAttempt(_ context.Context, id uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.smsCodes[id]
	if !ok {
		return 0, ErrNotFound
	}
	if c.Attempts < maxSMSAttemptsStored {
		c.Attempts++
		m.smsCodes[id] = c
	}
	return c.Attempts, nil
}

// ConsumeSMSCode implements Store.
func (m *MemStore) ConsumeSMSCode(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.smsCodes[id]
	if !ok || c.ConsumedAt != nil {
		return false, nil
	}
	c.ConsumedAt = &at
	m.smsCodes[id] = c
	return true, nil
}

// MarkSMSCodeSent implements Store.
func (m *MemStore) MarkSMSCodeSent(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.smsCodes[id]
	if !ok || c.SentAt != nil {
		return false, nil
	}
	c.SentAt = &at
	m.smsCodes[id] = c
	return true, nil
}

// DeleteEndedSMSCodes implements Store.
func (m *MemStore) DeleteEndedSMSCodes(_ context.Context, cutoff time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, c := range m.smsCodes {
		if n >= limit {
			break
		}
		if c.ExpiresAt.Before(cutoff) {
			delete(m.smsCodes, id)
			n++
		}
	}
	return n, nil
}

// DeleteUserSMSCodes implements Store.
func (m *MemStore) DeleteUserSMSCodes(_ context.Context, userID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, c := range m.smsCodes {
		if c.UserID == userID {
			delete(m.smsCodes, id)
			n++
		}
	}
	return n, nil
}
