package payments_test

import (
	"context"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/payments"
)

// withLateReauth puts r in front of the env's verifier (overload switch).
func withLateReauth(r *audittest.LateReauth[identity.ReauthProof]) envOption {
	return func(d *payments.Deps) { r.Next, d.Reauth = d.Reauth, r }
}

// reauthOp is one payments R operation on its fixture: the persona that
// runs it, the call and the denied record a refused proof must leave.
type reauthOp struct {
	persona string
	call    func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error
	denied  audit.Event // actor filled in by the test
}

// TestReauthDenied_EveryROperation (03 §6.2, BE-074): every payments R
// operation audits a refused proof — wrong (403 identity.reauth_failed) or
// missing (403 identity.reauth_required) — as one denied reauth_failed
// record of its action and resource (no IBAN, no amounts: nothing
// happened), writes nothing when identity cannot answer within the request
// budget (the bare deadline, httpx 503), and goes through with the right
// proof afterwards. Before BE-074 payments returned the refusal without a
// record.
func TestReauthDenied_EveryROperation(t *testing.T) {
	t.Parallel()
	accountIn := func(e *env) payments.AccountInput {
		return payments.AccountInput{Label: "Ana hesap", LegalPartyID: e.platform, IBAN: ibanKoro, HolderName: "Koro Teknoloji A.Ş.",
			BankName: "Örnek Bank"}
	}
	ref := func(id uuid.UUID) *uuid.UUID { return &id }
	cases := []struct {
		name   string
		action authz.Action
		opts   func(w *collectionLine) []envOption
		setup  func(t *testing.T, e *env, w *collectionLine) reauthOp
	}{
		{name: "createCollectionAccount", action: authz.PaymentsCollectionAccountCreate,
			setup: func(t *testing.T, e *env, _ *collectionLine) reauthOp {
				return reauthOp{persona: "finance_admin",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.CreateCollectionAccount(ctx, a, accountIn(e), p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeCollectionAccount}}
			}},
		{name: "decideCollectionAccount", action: authz.PaymentsCollectionAccountApprove,
			setup: func(t *testing.T, e *env, _ *collectionLine) reauthOp {
				c := e.createAccount(t, "finance_admin", ibanKoro)
				return reauthOp{persona: "platform_admin",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.DecideCollectionAccount(ctx, a, c.ID, payments.DecisionApproved, p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeCollectionAccount, ResourceID: ref(c.ID),
						Metadata: map[string]any{"decision": "approved"}}}
			}},
		{name: "retireCollectionAccount", action: authz.PaymentsCollectionAccountRetire,
			setup: func(t *testing.T, e *env, _ *collectionLine) reauthOp {
				c := e.activeAccount(t, ibanKoro, true)
				return reauthOp{persona: "platform_admin",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.RetireCollectionAccount(ctx, a, c.ID, p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeCollectionAccount, ResourceID: ref(c.ID),
						Metadata: map[string]any{"change": "retired"}}}
			}},
		{name: "makeDefaultCollectionAccount", action: authz.PaymentsCollectionAccountRetire,
			setup: func(t *testing.T, e *env, _ *collectionLine) reauthOp {
				c := e.activeAccount(t, ibanKoro, false)
				return reauthOp{persona: "finance_admin",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.MakeDefaultCollectionAccount(ctx, a, c.ID, p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeCollectionAccount, ResourceID: ref(c.ID),
						Metadata: map[string]any{"change": "made_default"}}}
			}},
		{name: "approveRefund", action: authz.PaymentsRefundApprove,
			setup: func(t *testing.T, e *env, _ *collectionLine) reauthOp {
				po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
				r, err := e.request(t, "finance_admin_2", lineRefund(po.payment.ID, po.lines[0].ID, 600_000))
				if err != nil {
					t.Fatal(err)
				}
				return reauthOp{persona: "finance_admin",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.ApproveRefund(ctx, a, r.ID, p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeRefund, ResourceID: ref(r.ID)}}
			}},
		{name: "cancelRefund", action: authz.PaymentsRefundCancel,
			setup: func(t *testing.T, e *env, _ *collectionLine) reauthOp {
				po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
				r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 600_000))
				return reauthOp{persona: "finance_admin",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.CloseRefund(ctx, a, r.ID, false, "Synthetic closure", p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeRefund, ResourceID: ref(r.ID)}}
			}},
		{name: "markRefundFailed", action: authz.PaymentsRefundMarkFailed,
			setup: func(t *testing.T, e *env, _ *collectionLine) reauthOp {
				po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
				r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 600_000))
				return reauthOp{persona: "finance_admin",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.CloseRefund(ctx, a, r.ID, true, "Synthetic failure", p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeRefund, ResourceID: ref(r.ID)}}
			}},
		{name: "approveReceivableCollection", action: authz.PaymentsReceivableApprove,
			opts: func(w *collectionLine) []envOption {
				return []envOption{withOrders(func(o payments.Orders) payments.Orders { w.Orders = o; return w })}
			},
			setup: func(t *testing.T, e *env, w *collectionLine) reauthOp {
				_, acct := collectionDebt(t, e, w)
				tr := e.collectionSource(t, acct, 540_000)
				fin := actor(t, "finance_admin")
				c, err := e.svc.CreateCollection(ctxOf(t, fin), fin, payments.CollectionInput{IncomingTransferID: tr.ID, OrderLineID: w.line})
				if err != nil {
					t.Fatal(err)
				}
				return reauthOp{persona: "finance_admin_2",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.ApproveCollection(ctx, a, c.ID, p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeReceivableCollection, ResourceID: ref(c.ID), OrgID: ref(c.OrgID)}}
			}},
		{name: "requestTransferReturn", action: authz.PaymentsTransferMarkReturned,
			setup: func(t *testing.T, e *env, _ *collectionLine) reauthOp {
				tr := e.transfer(t, e.activeAccount(t, ibanKoro, true), 77_000, "tanımsız", true)
				return reauthOp{persona: "finance_admin",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.RequestReturn(ctx, a, tr.ID, payments.ReturnRequest{Reason: "Sipariş yok"}, p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeIncomingTransfer, ResourceID: ref(tr.ID),
						Metadata: map[string]any{"step": "request"}}}
			}},
		{name: "decideTransferReturn", action: authz.PaymentsTransferMarkReturned,
			setup: func(t *testing.T, e *env, _ *collectionLine) reauthOp {
				tr := e.requestedReturn(t)
				return reauthOp{persona: "finance_admin_2",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.DecideReturn(ctx, a, tr.ID, payments.DecisionApproved, p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeIncomingTransfer, ResourceID: ref(tr.ID),
						Metadata: map[string]any{"step": "decision", "decision": "approved"}}}
			}},
		{name: "markTransferReturned", action: authz.PaymentsTransferMarkReturned,
			setup: func(t *testing.T, e *env, _ *collectionLine) reauthOp {
				tr := e.requestedReturn(t)
				fin2 := actor(t, "finance_admin_2")
				if _, err := e.svc.DecideReturn(ctxOf(t, fin2), fin2, tr.ID, payments.DecisionApproved, good); err != nil {
					t.Fatal(err)
				}
				receipt := e.files.add(files.PurposeRefundReceipt)
				return reauthOp{persona: "finance_admin",
					call: func(ctx context.Context, a authz.Actor, p identity.ReauthProof) error {
						_, err := e.svc.MarkReturned(ctx, a, tr.ID, payments.ReturnReceipt{BankReference: "EFT-1", ReceiptFileID: receipt}, p)
						return err
					},
					denied: audit.Event{ResourceType: authz.TypeIncomingTransfer, ResourceID: ref(tr.ID),
						Metadata: map[string]any{"step": "receipt"}}}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			late, w := &audittest.LateReauth[identity.ReauthProof]{}, &collectionLine{}
			opts := []envOption{withLateReauth(late)}
			if tc.opts != nil {
				opts = append(opts, tc.opts(w)...)
			}
			e := newEnv(t, opts...)
			op := tc.setup(t, e, w)
			a := actor(t, op.persona)
			op.denied.Action = string(tc.action)
			op.denied = op.denied.By(a)
			audittest.ReauthCase[identity.ReauthProof]{
				Call: func(ctx context.Context, p identity.ReauthProof) error {
					return op.call(authz.WithActor(ctx, a), a, p)
				},
				Rows:   func() []audittest.Row { return e.audit.reauthRows(tc.action) },
				Denied: rowOf(op.denied), Late: late,
				Wrong: identity.ReauthProof{Password: "wrong-password"}, WrongErr: identity.ErrReauthFailed,
				Missing: identity.ReauthProof{}, MissingErr: identity.ErrReauthRequired,
				Good: good,
			}.Run(t)
		})
	}
}

// ofOutcome returns the recorded events of act with outcome o.
func (l *auditLog) ofOutcome(act authz.Action, o audit.Outcome) []audit.Event {
	var out []audit.Event
	for _, e := range l.of(act) {
		if e.Outcome == o {
			out = append(out, e)
		}
	}
	return out
}

// requestedReturn is an unmatched transfer (with the sender's IBAN) whose
// return finance_admin requested.
func (e *env) requestedReturn(t *testing.T) payments.IncomingTransfer {
	t.Helper()
	tr := e.transfer(t, e.activeAccount(t, ibanKoro, true), 77_000, "tanımsız", true)
	fin := actor(t, "finance_admin")
	if _, err := e.svc.RequestReturn(ctxOf(t, fin), fin, tr.ID, payments.ReturnRequest{Reason: "Sipariş yok"}, good); err != nil {
		t.Fatal(err)
	}
	return tr
}

// rowOf is e in audittest's terms.
func rowOf(e audit.Event) audittest.Row {
	return audittest.Row{Action: e.Action, Outcome: string(e.Outcome), Reason: e.Reason, ActorUserID: e.ActorUserID,
		ActorOrgID: e.ActorOrgID, SystemJob: e.SystemJob, OrgID: e.OrgID, ResourceType: e.ResourceType, ResourceID: e.ResourceID,
		Metadata: e.Metadata}
}

// reauthRows returns the recorded events of act in audittest's terms.
func (l *auditLog) reauthRows(act authz.Action) []audittest.Row {
	var out []audittest.Row
	for _, e := range l.of(act) {
		out = append(out, rowOf(e))
	}
	return out
}
