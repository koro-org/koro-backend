//go:build integration

package orders_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/queue"
)

// pgDirectPayees serves the payee reads of the direct instructions from the
// real parties module over PostgreSQL (BE-109): the frozen payee and its
// current approved bank account. The checkout's DefaultFor and IsActive
// stay the env's fake (they run on the env's fake clock).
type pgDirectPayees struct {
	orders.Payees
	real *parties.Module
}

func (p pgDirectPayees) Get(ctx context.Context, id uuid.UUID) (parties.Payee, error) {
	return p.real.Payees.Get(ctx, id)
}

func (p pgDirectPayees) ApprovedAccount(ctx context.Context, id uuid.UUID) (parties.BankAccount, error) {
	return p.real.Payees.ApprovedAccount(ctx, id)
}

// pgDirectParties keeps the env's verified buyer party and reads any other
// party (the payee's foundation) from PostgreSQL.
type pgDirectParties struct {
	orders.Parties
	real *parties.Module
}

func (p pgDirectParties) Get(ctx context.Context, id uuid.UUID) (parties.LegalParty, error) {
	lp, err := p.Parties.Get(ctx, id)
	if errors.Is(err, parties.ErrNotFound) {
		return p.real.Service.Get(ctx, id)
	}
	return lp, err
}

// acceptReauth accepts every proof (the revocation's R step is not under
// test here).
type acceptReauth struct{}

func (acceptReauth) Verify(context.Context, uuid.UUID, identity.ReauthProof) (string, error) {
	return "totp", nil
}

// TestGetOrder_ModelC_RevokedAccount_Postgres (BE-109): a model C order
// waits for payment; finance revokes the payee's approved bank account
// through parties on PostgreSQL (approved → revoked under the table's
// CHECKs). getOrder still answers the order to the buyer administrator,
// unavailable and without any account; the payment instructions answer
// 409 parties.no_approved_account; the buyer can still cancel.
func TestGetOrder_ModelC_RevokedAccount_Postgres(t *testing.T) {
	t.Parallel()
	var pm parties.Module
	p := newPGEnv(t, func(d *orders.Deps) {
		d.Payees = pgDirectPayees{Payees: d.Payees, real: &pm}
		d.Parties = pgDirectParties{Parties: d.Parties, real: &pm}
		d.CollectionAccount = func(context.Context) (orders.CollectionAccount, bool, error) {
			return orders.CollectionAccount{IBAN: pgIBAN(), HolderName: "Synthetic Koro"}, true, nil
		}
	})
	var err error
	pm, err = parties.New(parties.Deps{Store: parties.NewGormStore(p.d.ORM), Tx: p.d.Tx, Audit: &audittest.Recorder{}, Idempotency: p.keeper,
		Orgs: newFakeOrgs(), Clock: p.clk, Log: discardLog(), Outbox: p.outbox, Queue: queue.NewMem(), Reauth: acceptReauth{}})
	if err != nil {
		t.Fatal(err)
	}
	// The payee's approved account: submitted by one admin, approved by another.
	file, acct, iban := uuid.NewV7(), uuid.NewV7(), pgIBAN()
	pgExec(t, p.d, `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime) VALUES ($1, 'bank_account_evidence', 'koro-files-dev', $2, 'application/pdf')`,
		file, "bank_account_evidence/2026/10/"+file.String())
	pgExec(t, p.d, `INSERT INTO payee_bank_accounts (id, payee_id, org_id, iban, holder_name, evidence_file_id, status, submitted_by, submitted_by_admin)
		VALUES ($1, $2, $3, $4, 'Synthetic Foundation', $5, 'pending_approval', $6, true)`, acct, p.seller.payee, p.seller.org, iban, file, pgUser(t, p.d))
	pgExec(t, p.d, `UPDATE payee_bank_accounts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`, acct, pgUser(t, p.d))

	off := p.w.offering(t, p.seller)
	pgExec(t, p.d, `UPDATE commerce_policies SET valid_to = $2 WHERE id = $1`, off.policy, policyFrom.Add(time.Hour))
	pgExec(t, p.d, `INSERT INTO commerce_policies (id, scope, scope_id, model_code, collection_method, commission_rate_bp, commission_basis,
		commission_payer, valid_from, created_by)
		VALUES ($1, 'offering', $2, 'C', 'direct_to_payee', 1000, 'net_excl_vat', 'buyer', $3, $4)`,
		uuid.NewV7(), off.id, policyFrom.Add(time.Hour), p.buyer.user)
	ctx := p.buyerActor(t)
	buyer, err := authz.ActorFrom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := p.create(ctx, line(off.id, 1))
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	o := vs[0].Order
	v, err := p.mod.Service.GetOrder(ctx, buyer, o.ID)
	if err != nil || v.DirectStatus != orders.DirectAvailable || len(v.DirectPayments) != 1 ||
		v.DirectPayments[0].IBAN != iban || v.DirectPayments[0].Amount != o.DirectToPayeeAmount {
		t.Fatalf("approved account: status=%q direct=%d err=%v", v.DirectStatus, len(v.DirectPayments), err)
	}

	fin := actor(t, "finance_admin")
	fin.UserID = pgUser(t, p.d)
	if _, err := pm.Service.AdminRevokeBankAccount(ctxOf(t, fin), fin, acct, "Synthetic revocation", identity.ReauthProof{TOTPCode: "000000"}); err != nil {
		t.Fatalf("AdminRevokeBankAccount: %v", err)
	}
	v, err = p.mod.Service.GetOrder(ctx, buyer, o.ID)
	if err != nil {
		t.Fatalf("GetOrder after the revocation: %v", err)
	}
	if v.DirectStatus != orders.DirectUnavailable || len(v.DirectPayments) != 0 || v.Order.ID != o.ID || len(v.Lines) != 1 {
		t.Fatalf("revoked: status=%q direct=%d order=%+v", v.DirectStatus, len(v.DirectPayments), v.Order)
	}
	_, err = p.mod.Service.PaymentInstructions(ctx, buyer, o.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.no_approved_account")
	if _, err := p.mod.Service.CancelOrder(ctx, buyer, o.ID, "payee account revoked"); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
}
