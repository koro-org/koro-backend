//go:build integration

package migrations_test

import (
	"strings"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_UserPhones (BE-172, migration 000177): the verified mobile
// number of an account and the one-time SMS codes:
//   - a Turkish mobile number in E.164 only, one per account, one account
//     per number (user_phones_phone_uq);
//   - an SMS code: a known purpose, a 32-byte MAC, 0–10 attempts, an
//     expiry after its creation, the users FK;
//   - app_rw reads, writes and deletes both (hard delete on erasure and on
//     removal);
//   - the down migration drops both tables and the sms_code policy, the up
//     one brings them back.
func TestSchema_UserPhones(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	check := func(t *testing.T) {
		t.Helper()
		u, v := w.user(), w.user()
		number := "+90555" + digits7(w.n())
		const phone = `INSERT INTO user_phones (user_id, phone_e164, verified_at) VALUES ($1, $2, now())`
		const code = `INSERT INTO sms_codes (id, user_id, purpose, phone_e164, key_id, code_hash, attempts, expires_at, created_at)
			VALUES ($1, $2, $3, $4, 'k1', $5, $6, now() + $7::interval, now())`
		mac := make([]byte, 32)
		expectOK(t, w.d.Pool, []p1aCase{
			{name: "a verified number", sql: phone, args: []any{u, number}},
			{name: "a sign-in code", sql: code, args: []any{uuid.NewV7(), u, "login", number, mac, 0, "5 minutes"}},
			{name: "a verification code of another number", sql: code, args: []any{uuid.NewV7(), v, "phone_verify", number, mac, 10, "10 minutes"}},
			{name: "the SMS second step on", sql: `UPDATE user_phones SET sms_mfa_enabled_at = now() WHERE user_id = $1`, args: []any{u}},
		})
		chk, fk := db.CodeCheckViolation, db.CodeForeignKeyViolation
		expectFail(t, w.d.Pool, []p1aCase{
			{name: "a second account on the number", sql: phone, args: []any{v, number}, state: db.CodeUniqueViolation,
				constraint: "user_phones_phone_uq"},
			{name: "a second number of the account", sql: phone, args: []any{u, "+905559999977"}, state: db.CodeUniqueViolation,
				constraint: "user_phones_pkey"},
			{name: "a landline", sql: phone, args: []any{v, "+902120000001"}, state: chk, constraint: "user_phones_phone_e164_chk"},
			{name: "a national form", sql: phone, args: []any{v, "05550000001"}, state: chk, constraint: "user_phones_phone_e164_chk"},
			{name: "an unknown account", sql: phone, args: []any{uuid.NewV7(), "+905559999978"}, state: fk, constraint: "user_phones_user_id_fkey"},
			{name: "an unknown purpose", sql: code, args: []any{uuid.NewV7(), u, "email", number, mac, 0, "5 minutes"}, state: chk,
				constraint: "sms_codes_purpose_chk"},
			{name: "a short MAC", sql: code, args: []any{uuid.NewV7(), u, "login", number, make([]byte, 16), 0, "5 minutes"}, state: chk,
				constraint: "sms_codes_code_hash_chk"},
			{name: "eleven attempts", sql: code, args: []any{uuid.NewV7(), u, "login", number, mac, 11, "5 minutes"}, state: chk,
				constraint: "sms_codes_attempts_chk"},
			{name: "expired at creation", sql: code, args: []any{uuid.NewV7(), u, "login", number, mac, 0, "0 minutes"}, state: chk,
				constraint: "sms_codes_expiry_chk"},
			{name: "a landline code", sql: code, args: []any{uuid.NewV7(), u, "login", "+902120000001", mac, 0, "5 minutes"}, state: chk,
				constraint: "sms_codes_phone_e164_chk"},
			{name: "a code of an unknown account", sql: code, args: []any{uuid.NewV7(), uuid.NewV7(), "login", number, mac, 0, "5 minutes"},
				state: fk, constraint: "sms_codes_user_id_fkey"},
		})
		// app_rw deletes both (removal, erasure, retention).
		w.exec(`DELETE FROM sms_codes WHERE user_id IN ($1, $2)`, u, v)
		w.exec(`DELETE FROM user_phones WHERE user_id = $1`, u)
		if n := w.count(`SELECT count(*) FROM user_phones WHERE user_id = $1`, u); n != 0 {
			t.Fatalf("the number survived its deletion (%d)", n)
		}
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000177_create_user_phones.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('user_phones', 'sms_codes')`) +
		w.count(`SELECT count(*) FROM retention_policies WHERE code = 'sms_code'`); n != 0 {
		t.Fatalf("down left %d objects of 000177", n)
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000177_create_user_phones.up.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM retention_policies WHERE code = 'sms_code' AND target = 'sms_codes' AND NOT legal_approved`); n != 1 {
		t.Fatalf("sms_code policy rows after up = %d", n)
	}
	check(t)
}

// TestSchema_SMSCodesUserCreatedIndex (BE-181, migration 000181): the usage
// of an account's SMS codes in a window reads a full index, consumed codes
// included.
func TestSchema_SMSCodesUserCreatedIndex(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	def := queryString(t, w.d, `SELECT indexdef FROM pg_indexes WHERE indexname = 'sms_codes_user_created_idx'`)
	if !strings.Contains(def, "(user_id, purpose, created_at)") || strings.Contains(def, "WHERE") {
		t.Fatalf("indexdef = %q", def)
	}
}

// TestSchema_SMSResetPreference (BE-186, migration 000182): the password reset
// by SMS of an account:
//   - off by default, a date when on, never next to the SMS second step
//     (user_phones_sms_reset_sms_mfa_chk);
//   - sms_codes takes the purpose phone_change;
//   - the down migration drops the column, the constraint and the codes of the
//     purpose; the up one brings them back.
func TestSchema_SMSResetPreference(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	const code = `INSERT INTO sms_codes (id, user_id, purpose, phone_e164, key_id, code_hash, attempts, expires_at, created_at)
		VALUES ($1, $2, 'phone_change', $3, 'k1', $4, 0, now() + interval '10 minutes', now())`
	check := func(t *testing.T) {
		t.Helper()
		u := w.user()
		number := "+90555" + digits7(w.n())
		w.exec(`INSERT INTO user_phones (user_id, phone_e164, verified_at) VALUES ($1, $2, now())`, u, number)
		if n := w.count(`SELECT count(*) FROM user_phones WHERE user_id = $1 AND sms_reset_enabled_at IS NULL`, u); n != 1 {
			t.Fatalf("a new number with the reset on (%d)", n)
		}
		expectOK(t, w.d.Pool, []p1aCase{
			{name: "the reset on", sql: `UPDATE user_phones SET sms_reset_enabled_at = now() WHERE user_id = $1`, args: []any{u}},
			{name: "a code of the current number", sql: code, args: []any{uuid.NewV7(), u, number, make([]byte, 32)}},
		})
		expectFail(t, w.d.Pool, []p1aCase{
			{name: "the second step next to the reset", sql: `UPDATE user_phones SET sms_mfa_enabled_at = now() WHERE user_id = $1`, args: []any{u},
				state: db.CodeCheckViolation, constraint: "user_phones_sms_reset_sms_mfa_chk"},
		})
		expectOK(t, w.d.Pool, []p1aCase{
			{name: "the reset off", sql: `UPDATE user_phones SET sms_reset_enabled_at = NULL WHERE user_id = $1`, args: []any{u}},
			{name: "then the second step", sql: `UPDATE user_phones SET sms_mfa_enabled_at = now() WHERE user_id = $1`, args: []any{u}},
		})
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000182_sms_reset_preference.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'user_phones' AND column_name = 'sms_reset_enabled_at'`) +
		w.count(`SELECT count(*) FROM sms_codes WHERE purpose = 'phone_change'`); n != 0 {
		t.Fatalf("down left %d objects of 000182", n)
	}
	u := w.user()
	expectFail(t, w.d.Pool, []p1aCase{
		{name: "the purpose after down", sql: code, args: []any{uuid.NewV7(), u, "+905559999976", make([]byte, 32)}, state: db.CodeCheckViolation,
			constraint: "sms_codes_purpose_chk"},
	})
	if _, err := owner.Exec(t.Context(), mustRead(t, "000182_sms_reset_preference.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}

// digits7 is seven digits of a counter value (unique numbers per run).
func digits7(n string) string {
	d := []byte("0000000" + n)
	return string(d[len(d)-7:])
}
