//go:build integration

package seed_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"koro/db/seed"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

func plannedTotal(p *seed.Plan) int {
	total := 0
	for _, n := range p.Counts() {
		total += n
	}
	return total
}

func collectionExec(t *testing.T, d *db.DB, sql string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func collectionSnapshot(t *testing.T, d *db.DB) string {
	t.Helper()
	var value string
	if err := d.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'parties', (SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM legal_parties p WHERE kind = 'platform'),
 'accounts', (SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM collection_accounts a))::text`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func applyCollection(t *testing.T, d *db.DB, p *seed.Plan) seed.Result {
	t.Helper()
	result, err := seed.Apply(t.Context(), d.Pool, p)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSeedDemo_CollectionAccount(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ref := seedCommerceReference(t, d)
	p := build(t, ref, now)
	applyCollection(t, d, p)
	var count int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM collection_accounts a
 JOIN legal_parties p ON p.id = a.legal_party_id
 JOIN users creator ON creator.id = a.created_by JOIN users approver ON approver.id = a.approved_by
 WHERE a.status = 'active' AND a.is_default AND a.approved_by <> a.created_by AND a.approved_at IS NOT NULL
 AND a.iban = $1 AND koro_iban_tr_valid(a.iban) AND a.holder_name LIKE 'ÖRNEK%' AND a.bank_name LIKE 'ÖRNEK%'
 AND p.kind = 'platform' AND p.status = 'verified' AND p.title LIKE 'ÖRNEK%'
 AND creator.email = $2 AND approver.email = $3 AND creator.password_hash IS NULL AND approver.password_hash IS NULL
 AND EXISTS (SELECT 1 FROM platform_role_assignments r WHERE r.user_id = creator.id AND r.role = 'finance_admin' AND r.status = 'active')
 AND EXISTS (SELECT 1 FROM platform_role_assignments r WHERE r.user_id = approver.id AND r.role = 'finance_admin' AND r.status = 'active')`,
		seed.CollectionIBAN, seed.FinanceEmail1, seed.FinanceEmail2).Scan(&count); err != nil || count != 1 {
		t.Fatalf("valid four-eye default account = %d (%v)", count, err)
	}
	before := collectionSnapshot(t, d)
	result := applyCollection(t, d, build(t, ref, now.Add(48*time.Hour)))
	if inserted, kept := result.Total(); inserted != 0 || kept != plannedTotal(p) {
		t.Fatalf("second Apply inserted %d, kept %d", inserted, kept)
	}
	if collectionSnapshot(t, d) != before {
		t.Fatal("second Apply changed collection setup")
	}
	// Real database protection remains enabled; the seed did not bypass it.
	_, err := d.Pool.Exec(t.Context(), `UPDATE collection_accounts SET approved_by = created_by`)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.ConstraintName != "collection_accounts_four_eyes_chk" {
		t.Fatalf("same-eye update: %v", err)
	}
	if collectionSnapshot(t, d) != before {
		t.Fatal("rejected same-eye update persisted")
	}
}

func TestSeedDemo_CollectionPreservesOperatorChoices(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"active_nondefault", "retired", "rejected", "pending_approval"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			d := testutil.NewDB(t)
			ref := seedCommerceReference(t, d)
			applyCollection(t, d, build(t, ref, now))
			collectionExec(t, d, `UPDATE collection_accounts SET is_default = false, label = 'ÖRNEK Operatör seçimi'`)
			switch state {
			case "retired":
				collectionExec(t, d, `UPDATE collection_accounts SET status = 'retired', retired_at = now()`)
			case "rejected":
				collectionExec(t, d, `UPDATE collection_accounts SET status = 'rejected'`)
			case "pending_approval":
				collectionExec(t, d, `UPDATE collection_accounts SET status = 'pending_approval', approved_by = NULL, approved_at = NULL`)
			}
			collectionExec(t, d, `UPDATE legal_parties SET title = 'ÖRNEK Operatör unvanı', status = 'inactive', verified_at = NULL, verified_by = NULL WHERE kind = 'platform'`)
			before := collectionSnapshot(t, d)
			result := applyCollection(t, d, build(t, ref, now.Add(48*time.Hour)))
			if inserted, _ := result.Total(); inserted != 0 {
				t.Fatalf("inserted %d after operator choice", inserted)
			}
			if collectionSnapshot(t, d) != before {
				t.Fatal("operator setup changed")
			}
		})
	}
}

func TestSeedDemo_CollectionExistingSetup(t *testing.T) {
	t.Parallel()
	for _, setup := range []string{"platform_only", "operator_default", "pending_same_iban", "operator_retired", "operator_nondefault", "operator_pending", "operator_rejected"} {
		t.Run(setup, func(t *testing.T) {
			t.Parallel()
			d := testutil.NewDB(t)
			ref := seedCommerceReference(t, d)
			p := build(t, ref, now)
			// Emulate a database seeded before BE-065; all ordinary A/C rows exist.
			applyCollection(t, d, &seed.Plan{Rows: p.Rows, After: p.After})
			collectionExec(t, d, `INSERT INTO legal_parties (kind,legal_type,org_id,title,status,created_by)
    SELECT 'platform','company',$1,'ÖRNEK Mevcut platform','draft',id FROM users WHERE email = $2`, ref.PlatformOrgID, seed.FinanceEmail1)
			if setup != "platform_only" {
				fixtureIBAN := seed.CollectionIBAN
				if setup != "pending_same_iban" {
					fixtureIBAN = "TR470000000000000000000000"
				}
				collectionExec(t, d, `INSERT INTO collection_accounts (label,legal_party_id,iban,holder_name,bank_name,created_by)
     SELECT 'ÖRNEK Mevcut hesap',p.id,$1,'ÖRNEK Operatör','ÖRNEK Banka',u.id FROM legal_parties p CROSS JOIN users u
     WHERE p.kind = 'platform' AND u.email = $2`, fixtureIBAN, seed.FinanceEmail1)
				if setup != "operator_pending" && setup != "pending_same_iban" {
					collectionExec(t, d, `UPDATE collection_accounts SET status = 'active', is_default = true,
      approved_by = (SELECT id FROM users WHERE email = $1), approved_at = now()`, seed.FinanceEmail2)
				}
				// Model an operator deliberately removing the last default;
				// these accounts have neither the demo ID nor its IBAN.
				switch setup {
				case "operator_retired":
					collectionExec(t, d, `UPDATE collection_accounts SET status = 'retired', is_default = false, retired_at = now()`)
				case "operator_nondefault":
					collectionExec(t, d, `UPDATE collection_accounts SET is_default = false`)
				case "operator_rejected":
					collectionExec(t, d, `UPDATE collection_accounts SET status = 'rejected', is_default = false`)
				}
			}
			before := collectionSnapshot(t, d)
			result := applyCollection(t, d, p)
			if setup != "platform_only" {
				if collectionSnapshot(t, d) != before {
					t.Fatal("operator's existing account/party changed")
				}
				if added, _ := result.Total(); added != 0 {
					t.Fatalf("created unnecessary rows: %d", added)
				}
			} else {
				var n int
				if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM collection_accounts a JOIN legal_parties p ON p.id = a.legal_party_id
     WHERE a.is_default AND a.status = 'active' AND p.title = 'ÖRNEK Mevcut platform' AND p.status = 'draft'`).Scan(&n); err != nil || n != 1 {
					t.Fatalf("existing platform preserved/reused = %d (%v)", n, err)
				}
				if added, _ := result.Total(); added != 1 {
					t.Fatalf("new account only: %d inserted", added)
				}
			}
		})
	}
}

func TestSeedDemo_CollectionConcurrentApply(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	p := build(t, seedCommerceReference(t, d), now)
	type outcome struct {
		result seed.Result
		err    error
	}
	outcomes := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			ready.Wait()
			result, err := seed.Apply(t.Context(), d.Pool, p)
			outcomes <- outcome{result, err}
		}()
	}
	inserted, kept := 0, 0
	for range 2 {
		out := <-outcomes
		if out.err != nil {
			t.Fatal(out.err)
		}
		a, b := out.result.Total()
		inserted += a
		kept += b
	}
	if inserted != plannedTotal(p) || kept != plannedTotal(p) {
		t.Fatalf("concurrent Apply: %d inserted, %d existing", inserted, kept)
	}
}

func TestSeedDemo_CollectionDoesNotReinstateRevokedFinance(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	p := build(t, seedCommerceReference(t, d), now)
	applyCollection(t, d, &seed.Plan{Rows: p.Rows, After: p.After})
	collectionExec(t, d, `UPDATE platform_role_assignments SET status = 'revoked', revoked_at = now(), revoked_by = approved_by,
 revoke_requested_by = requested_by, revoke_requested_at = now(), revoke_reason = 'ÖRNEK operator revocation'
 WHERE user_id = (SELECT id FROM users WHERE email = $1)`, seed.FinanceEmail2)
	_, err := seed.Apply(t.Context(), d.Pool, p)
	if err == nil || !strings.Contains(err.Error(), "two active finance demo accounts") {
		t.Fatalf("revoked approver: %v", err)
	}
	var n int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM collection_accounts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("account without finance eyes = %d (%v)", n, err)
	}
}
