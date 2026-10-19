package payments

import (
	"context"
	"strings"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

func collectionResource(c ReceivableCollection) authz.Resource {
	return authz.Resource{Type: authz.TypeReceivableCollection, ID: c.ID, Attrs: map[string]string{
		authz.AttrStatus: string(c.Status), authz.AttrRequestedBy: c.RequestedBy.String()}}
}
func collectionAudit(a authz.Actor, act authz.Action, c ReceivableCollection, reason string, meta map[string]any) audit.Event {
	if meta == nil {
		meta = map[string]any{}
	}
	meta["incoming_transfer_id"], meta["order_line_id"], meta["payee_id"] = c.IncomingTransferID.String(), c.OrderLineID.String(), c.PayeeID.String()
	meta["amount"], meta["status"], meta["collection_account_id"] = c.Amount, string(c.Status), c.CollectionAccountID.String()
	id, org := c.ID, c.OrgID
	return audit.Event{Action: string(act), Outcome: audit.Success, ResourceType: authz.TypeReceivableCollection, ResourceID: &id, OrgID: &org, Reason: reason, Metadata: meta}.By(a)
}

func (s *service) ListCollections(ctx context.Context, a authz.Actor, q CollectionQuery) (CollectionPage, error) {
	if err := authz.Can(ctx, a, authz.PaymentsReceivableList, adminSurface); err != nil {
		return CollectionPage{}, err
	}
	if q.invalid != nil {
		return CollectionPage{}, q.invalid
	}
	if q.Filter.Status != "" && !q.Filter.Status.Valid() {
		return CollectionPage{}, fieldErr("query.status", "is not a collection status")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListCollections(ctx, q.Filter, q.After, limit+1)
	if err != nil {
		return CollectionPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := CollectionPage{Items: rows}
	if more {
		last := rows[len(rows)-1]
		out.Next = &TimeCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// CreateCollection reserves an existing bank receipt and a line's outstanding
// clawback. Lock order is transfer -> payee advisory -> parent FK locks; refund
// completion also takes the payee lock before locking orders and lines.
func (s *service) CreateCollection(ctx context.Context, a authz.Actor, in CollectionInput) (ReceivableCollection, error) {
	if err := authz.Can(ctx, a, authz.PaymentsReceivableCreate, adminSurface); err != nil {
		return ReceivableCollection{}, err
	}
	if in.IncomingTransferID == uuid.Nil() || in.OrderLineID == uuid.Nil() {
		return ReceivableCollection{}, fieldErr("body", "incoming_transfer_id and order_line_id are required")
	}
	if s.d.LedgerLines == nil {
		return ReceivableCollection{}, ErrUnavailable
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		Input CollectionInput
	}{"create_receivable_collection", in})
	if err != nil {
		return ReceivableCollection{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		t, err := s.d.Store.GetTransfer(ctx, in.IncomingTransferID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if t.Status != TransferUnmatched {
			return uuid.Nil(), ErrInvalidState
		}
		if t.ReceiptFileID == nil || strings.TrimSpace(t.BankReference) == "" {
			return uuid.Nil(), fieldErr("body.incoming_transfer_id", "requires a bank reference and transfer receipt")
		}
		if err := s.checkFile(ctx, "body.incoming_transfer_id", *t.ReceiptFileID, files.PurposeTransferReceipt); err != nil {
			return uuid.Nil(), err
		}
		l, err := s.d.Orders.GetLine(ctx, in.OrderLineID)
		if err != nil {
			return uuid.Nil(), err
		}
		if l.PayeeID == nil || l.PayoutID == nil {
			return uuid.Nil(), ErrInvalidState
		}
		o, err := s.d.Orders.ForPayment(ctx, l.OrderID)
		if err != nil {
			return uuid.Nil(), err
		}
		if o.ModelCode != string(ledger.ModelA) || o.CollectionMethod == orders.CollectDirectToPayee {
			return uuid.Nil(), ErrInvalidState
		}
		if err := s.d.LedgerLines.LockPayees(ctx, []uuid.UUID{*l.PayeeID}); err != nil {
			return uuid.Nil(), err
		}
		if err := s.collectionLimit(ctx, l.ID, uuid.Nil(), t.Amount); err != nil {
			return uuid.Nil(), err
		}
		now := s.now()
		c := ReceivableCollection{ID: uuid.NewV7(), IncomingTransferID: t.ID, OrderLineID: l.ID, OrderID: l.OrderID, PayeeID: *l.PayeeID, OrgID: l.SellerOrgID,
			CollectionAccountID: t.CollectionAccountID, Amount: t.Amount, BankReference: t.BankReference, ReceiptFileID: *t.ReceiptFileID,
			Status: CollectionRequested, RequestedBy: a.UserID, CreatedAt: now, UpdatedAt: now}
		if err := s.d.Store.InsertCollection(ctx, c); err != nil {
			return uuid.Nil(), err
		}
		next := t
		next.Status = TransferCollectionRequested
		if _, err := s.d.Store.UpdateTransfer(ctx, next, TransferUnmatched); err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, collectionAudit(a, authz.PaymentsReceivableCreate, c, "", nil)); err != nil {
			return uuid.Nil(), err
		}
		return c.ID, nil
	})
	if err != nil {
		return ReceivableCollection{}, constraints.Translate(ledger.TranslateError(err))
	}
	return s.d.Store.GetCollection(ctx, id, false)
}

func (s *service) collectionLimit(ctx context.Context, lineID, self uuid.UUID, amount int64) error {
	outstanding, err := s.d.LedgerLines.LineReceivable(ctx, lineID)
	if err != nil {
		return err
	}
	pending, err := s.d.Store.PendingCollections(ctx, lineID, self)
	if err != nil {
		return err
	}
	if pending < 0 || outstanding < pending || amount > outstanding-pending {
		return ErrCollectionExceedsReceivable
	}
	return nil
}

// ApproveCollection posts received cash exactly once. The transfer is already
// claimed; it cannot be matched, returned, ignored or collected a second time.
func (s *service) ApproveCollection(ctx context.Context, a authz.Actor, id uuid.UUID, proof identity.ReauthProof) (ReceivableCollection, error) {
	if err := authz.Can(ctx, a, authz.PaymentsReceivableList, adminSurface); err != nil {
		return ReceivableCollection{}, err
	}
	c, err := s.d.Store.GetCollection(ctx, id, false)
	if err != nil {
		return ReceivableCollection{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsReceivableApprove, collectionResource(c)); err != nil {
		return ReceivableCollection{}, err
	}
	kind, err := s.reauth(ctx, a, proof)
	if err != nil {
		return ReceivableCollection{}, err
	}
	if s.d.LedgerLines == nil {
		return ReceivableCollection{}, ErrUnavailable
	}
	hash, err := idempotency.Hash(struct {
		Op string
		ID uuid.UUID
	}{"approve_receivable_collection", id})
	if err != nil {
		return ReceivableCollection{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetCollection(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != CollectionRequested {
			return uuid.Nil(), ErrInvalidState
		}
		if err := authz.Can(ctx, a, authz.PaymentsReceivableApprove, collectionResource(cur)); err != nil {
			return uuid.Nil(), err
		}
		t, err := s.d.Store.GetTransfer(ctx, cur.IncomingTransferID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if t.Status != TransferCollectionRequested || t.Amount != cur.Amount || t.CollectionAccountID != cur.CollectionAccountID || t.ReceiptFileID == nil || *t.ReceiptFileID != cur.ReceiptFileID || t.BankReference != cur.BankReference {
			return uuid.Nil(), ErrInvalidState
		}
		if err := s.checkFile(ctx, "body.incoming_transfer_id", cur.ReceiptFileID, files.PurposeTransferReceipt); err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.LedgerLines.LockPayees(ctx, []uuid.UUID{cur.PayeeID}); err != nil {
			return uuid.Nil(), err
		}
		if err := s.collectionLimit(ctx, cur.OrderLineID, cur.ID, cur.Amount); err != nil {
			return uuid.Nil(), err
		}
		now, by, line := s.now(), a.UserID, cur.OrderLineID
		txn, err := s.d.Ledger.Post(ctx, ledger.Txn{Kind: ledger.KindAdjustment, RefType: ledger.RefManual, RefID: cur.ID, OccurredAt: now,
			Entries: []ledger.Entry{{Account: ledger.AccountKoroBank, Amount: cur.Amount, OrderLineID: &line}, {Account: ledger.AccountPayeeReceivable, Amount: -cur.Amount, OrderLineID: &line}}})
		if err != nil {
			return uuid.Nil(), err
		}
		next := cur
		next.Status, next.ApprovedBy, next.ApprovedAt, next.LedgerTransactionID = CollectionCollected, &by, &now, &txn
		stored, err := s.d.Store.UpdateCollection(ctx, next)
		if err != nil {
			return uuid.Nil(), err
		}
		t.Status = TransferCollected
		if _, err := s.d.Store.UpdateTransfer(ctx, t, TransferCollectionRequested); err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, collectionAudit(a, authz.PaymentsReceivableApprove, stored, "", map[string]any{audit.MetaReauth: kind, "ledger_transaction_id": txn.String()})); err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, nil
	})
	if err != nil {
		return ReceivableCollection{}, constraints.Translate(ledger.TranslateError(err))
	}
	return s.d.Store.GetCollection(ctx, id, false)
}

// RejectCollection releases both reservations; the original rejected request
// remains immutable and the same transfer may be proposed against another line.
func (s *service) RejectCollection(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (ReceivableCollection, error) {
	if err := authz.Can(ctx, a, authz.PaymentsReceivableReject, adminSurface); err != nil {
		return ReceivableCollection{}, err
	}
	why, err := cleanText("body.note", reason, 1, 500)
	if err != nil {
		return ReceivableCollection{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		ID     uuid.UUID
		Reason string
	}{"reject_receivable_collection", id, why})
	if err != nil {
		return ReceivableCollection{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetCollection(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != CollectionRequested {
			return uuid.Nil(), ErrInvalidState
		}
		t, err := s.d.Store.GetTransfer(ctx, cur.IncomingTransferID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if t.Status != TransferCollectionRequested {
			return uuid.Nil(), ErrInvalidState
		}
		now, by := s.now(), a.UserID
		cur.Status, cur.RejectedAt, cur.RejectedBy, cur.RejectionReason = CollectionRejected, &now, &by, &why
		stored, err := s.d.Store.UpdateCollection(ctx, cur)
		if err != nil {
			return uuid.Nil(), err
		}
		t.Status = TransferUnmatched
		if _, err := s.d.Store.UpdateTransfer(ctx, t, TransferCollectionRequested); err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.d.Audit.Log(ctx, collectionAudit(a, authz.PaymentsReceivableReject, stored, why, nil))
	})
	if err != nil {
		return ReceivableCollection{}, err
	}
	return s.d.Store.GetCollection(ctx, id, false)
}
