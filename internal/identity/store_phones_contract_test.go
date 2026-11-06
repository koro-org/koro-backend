package identity_test

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/identity"
)

// phoneContract is the user_phones and sms_codes part of the store contract
// (BE-172; 000177): one number per account and one account per number, the
// SMS second step, the open code of a purpose, the attempt counter, single
// use, the sent mark, the retention cleanup and the erasure.
func phoneContract(t *testing.T, s identity.Store) {
	t.Helper()
	ctx := t.Context()
	u, v := contractUser(uuid.NewV7().String()+"@contract.test"), contractUser(uuid.NewV7().String()+"@contract.test")
	for _, x := range []identity.User{u, v} {
		if err := s.InsertUser(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	// Numbers unique per test run: the GORM world shares one database.
	n1 := "+90555" + digitsOnly(uuid.NewV7().String()[29:36])
	n2 := n1[:len(n1)-1] + string('0'+(n1[len(n1)-1]-'0'+1)%10)

	if _, found, err := s.Phone(ctx, u.ID); err != nil || found {
		t.Fatalf("Phone(none) = %v, %v", found, err)
	}
	at := contractBase.Add(time.Hour)
	if err := s.PutPhone(ctx, identity.Phone{UserID: u.ID, Number: n1, VerifiedAt: at}); err != nil {
		t.Fatal(err)
	}
	p, found, err := s.Phone(ctx, u.ID)
	if err != nil || !found || p.Number != n1 || !p.VerifiedAt.Equal(at) || p.SMSMFAEnabledAt != nil || p.CreatedAt.IsZero() {
		t.Fatalf("Phone = %+v, %v, %v", p, found, err)
	}
	if owner, found, err := s.PhoneOwner(ctx, n1); err != nil || !found || owner != u.ID {
		t.Fatalf("PhoneOwner = %v, %v, %v", owner, found, err)
	}
	if err := s.PutPhone(ctx, identity.Phone{UserID: v.ID, Number: n1, VerifiedAt: at}); !errors.Is(err, identity.ErrPhoneTaken) {
		t.Fatalf("a second account on the number = %v, want ErrPhoneTaken", err)
	}
	// The SMS second step, then a new number: the row is replaced, created_at stays.
	on := at.Add(time.Minute)
	if ok, err := s.SetSMSMFA(ctx, u.ID, &on); err != nil || !ok {
		t.Fatalf("SetSMSMFA(on) = %v, %v", ok, err)
	}
	if ok, err := s.SetSMSMFA(ctx, v.ID, &on); err != nil || ok {
		t.Fatalf("SetSMSMFA without a number = %v, %v", ok, err)
	}
	if err := s.PutPhone(ctx, identity.Phone{UserID: u.ID, Number: n2, VerifiedAt: on, SMSMFAEnabledAt: &on}); err != nil {
		t.Fatal(err)
	}
	q, _, _ := s.Phone(ctx, u.ID)
	if q.Number != n2 || q.SMSMFAEnabledAt == nil || !q.SMSMFAEnabledAt.Equal(on) || !q.CreatedAt.Equal(p.CreatedAt) {
		t.Fatalf("replaced phone = %+v (was %+v)", q, p)
	}
	if _, found, _ := s.PhoneOwner(ctx, n1); found {
		t.Fatal("the old number still names the account")
	}
	if err := s.PutPhone(ctx, identity.Phone{UserID: v.ID, Number: n1, VerifiedAt: at}); err != nil {
		t.Fatalf("the freed number: %v", err)
	}

	// SMS codes: the newest open one of a purpose, attempts, single use.
	code := func(purpose identity.SMSPurpose, created time.Time) identity.SMSCode {
		c := identity.SMSCode{ID: uuid.NewV7(), UserID: u.ID, Purpose: purpose, Number: n2, KeyID: "k1",
			CodeHash: bytes.Repeat([]byte{7}, 32), ExpiresAt: created.Add(10 * time.Minute), CreatedAt: created}
		if err := s.InsertSMSCode(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if _, found, err := s.OpenSMSCode(ctx, u.ID, identity.SMSLogin); err != nil || found {
		t.Fatalf("OpenSMSCode(none) = %v, %v", found, err)
	}
	old := code(identity.SMSLogin, at)
	cur := code(identity.SMSLogin, at.Add(time.Minute))
	reset := code(identity.SMSPasswordReset, at)
	if got, found, err := s.OpenSMSCode(ctx, u.ID, identity.SMSLogin); err != nil || !found || got.ID != cur.ID ||
		!bytes.Equal(got.CodeHash, cur.CodeHash) || got.Number != n2 || !got.ExpiresAt.Equal(cur.ExpiresAt) || !got.CreatedAt.Equal(cur.CreatedAt) {
		t.Fatalf("OpenSMSCode = %+v, %v, %v (want %s)", got, found, err, cur.ID)
	}
	for i := 1; i <= 11; i++ {
		n, err := s.AddSMSCodeAttempt(ctx, cur.ID)
		if err != nil || n != min(i, 10) {
			t.Fatalf("attempt %d = %d, %v", i, n, err)
		}
	}
	if _, err := s.AddSMSCodeAttempt(ctx, uuid.NewV7()); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("attempt of an unknown code = %v", err)
	}
	if ok, err := s.ConsumeSMSCode(ctx, cur.ID, at); err != nil || !ok {
		t.Fatalf("ConsumeSMSCode = %v, %v", ok, err)
	}
	if ok, _ := s.ConsumeSMSCode(ctx, cur.ID, at); ok {
		t.Fatal("a code consumed twice")
	}
	if got, found, _ := s.OpenSMSCode(ctx, u.ID, identity.SMSLogin); !found || got.ID != old.ID {
		t.Fatalf("after consuming the newest, the open code = %+v, %v", got, found)
	}
	if err := s.CloseOpenSMSCodes(ctx, u.ID, identity.SMSLogin, at); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.OpenSMSCode(ctx, u.ID, identity.SMSLogin); found {
		t.Fatal("CloseOpenSMSCodes left an open sign-in code")
	}
	if _, found, _ := s.OpenSMSCode(ctx, u.ID, identity.SMSPasswordReset); !found {
		t.Fatal("CloseOpenSMSCodes closed another purpose")
	}
	// Usage in a window: consumed codes count, the start is inclusive, the
	// other purposes and accounts do not.
	for _, c := range []struct {
		user    uuid.UUID
		purpose identity.SMSPurpose
		since   time.Time
		want    identity.SMSCodeUse
	}{
		{u.ID, identity.SMSLogin, at, identity.SMSCodeUse{Codes: 2, Attempts: 10}},
		{u.ID, identity.SMSLogin, at.Add(time.Minute), identity.SMSCodeUse{Codes: 1, Attempts: 10}},
		{u.ID, identity.SMSLogin, at.Add(2 * time.Minute), identity.SMSCodeUse{}},
		{u.ID, identity.SMSPasswordReset, at, identity.SMSCodeUse{Codes: 1}},
		{u.ID, identity.SMSPhoneVerify, at, identity.SMSCodeUse{}},
		{v.ID, identity.SMSLogin, at, identity.SMSCodeUse{}},
	} {
		if got, err := s.SMSCodeUsage(ctx, c.user, c.purpose, c.since); err != nil || got != c.want {
			t.Fatalf("SMSCodeUsage(%s, %s) = %+v, %v, want %+v", c.purpose, c.since, got, err, c.want)
		}
	}
	if ok, err := s.MarkSMSCodeSent(ctx, reset.ID, at); err != nil || !ok {
		t.Fatalf("MarkSMSCodeSent = %v, %v", ok, err)
	}
	if ok, _ := s.MarkSMSCodeSent(ctx, reset.ID, at); ok {
		t.Fatal("a code marked sent twice")
	}
	if got, err := s.SMSCode(ctx, reset.ID); err != nil || got.SentAt == nil || got.ConsumedAt != nil {
		t.Fatalf("SMSCode = %+v, %v", got, err)
	}
	if _, err := s.SMSCode(ctx, uuid.NewV7()); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unknown code = %v", err)
	}
	// Retention: codes that expired before the cutoff go, in batches.
	if n, err := s.DeleteEndedSMSCodes(ctx, at.Add(10*time.Minute+time.Second), 1); err != nil || n != 1 {
		t.Fatalf("DeleteEndedSMSCodes(batch 1) = %d, %v", n, err)
	}
	if n, err := s.DeleteEndedSMSCodes(ctx, at.Add(10*time.Minute+time.Second), 10); err != nil || n != 1 {
		t.Fatalf("DeleteEndedSMSCodes = %d, %v (the code created a minute later must stay)", n, err)
	}
	if _, err := s.SMSCode(ctx, cur.ID); err != nil {
		t.Fatalf("the later code went: %v", err)
	}
	// Erasure: the account's number and codes; the other account's stay.
	if n, err := s.DeleteUserSMSCodes(ctx, u.ID); err != nil || n != 1 {
		t.Fatalf("DeleteUserSMSCodes = %d, %v", n, err)
	}
	if n, err := s.DeletePhone(ctx, u.ID); err != nil || n != 1 {
		t.Fatalf("DeletePhone = %d, %v", n, err)
	}
	if n, _ := s.DeletePhone(ctx, u.ID); n != 0 {
		t.Fatal("DeletePhone twice")
	}
	if _, found, _ := s.Phone(ctx, v.ID); !found {
		t.Fatal("another account's number went")
	}
}

// digitsOnly maps the hex characters of an ID onto digits.
func digitsOnly(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c < '0' || c > '9' {
			b[i] = '0' + c%10
		}
	}
	return string(b)
}
