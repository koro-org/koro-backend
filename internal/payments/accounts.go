package payments

import (
	"context"
	"errors"
	"log/slog"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
)

// Changes of a collection account announced by the security notice task
// (NotifyAccountChangedTaskName; an approval has its own event).
const (
	changeMadeDefault = "made_default"
	changeRetired     = "retired"
)

// accountNotice is the payload of NotifyAccountChangedTaskName (IDs and a
// fixed code only).
type accountNotice struct {
	CollectionAccountID uuid.UUID `json:"collection_account_id"`
	Change              string    `json:"change"`
}

// ListCollectionAccounts implements Service: authz (403) → every account,
// newest first. Koro's IBANs are not personal data (they are printed on
// every payment instruction).
func (s *service) ListCollectionAccounts(ctx context.Context, a authz.Actor) ([]CollectionAccount, error) {
	if err := authz.Can(ctx, a, authz.PaymentsCollectionAccountList, adminSurface); err != nil {
		return nil, err
	}
	return s.d.Store.ListAccounts(ctx)
}

// CreateCollectionAccount implements Service (fin·R): authz (403) → body
// (422; the IBAN: parties.iban_invalid) → the party must be Koro's own
// (kind platform; 422) → the re-authentication proof (403) → in one
// transaction: the account (pending_approval; 409 payments.iban_in_use when
// another open account has the IBAN) → audit. Nobody can pay into it until
// another admin approves it and it is made the default.
func (s *service) CreateCollectionAccount(ctx context.Context, a authz.Actor, in AccountInput, proof identity.ReauthProof) (CollectionAccount, error) {
	if err := authz.Can(ctx, a, authz.PaymentsCollectionAccountCreate, adminSurface); err != nil {
		return CollectionAccount{}, err
	}
	label, err := cleanText("body.label", in.Label, 1, 100)
	if err != nil {
		return CollectionAccount{}, err
	}
	number, err := cleanIBAN("body.iban", in.IBAN)
	if err != nil {
		return CollectionAccount{}, err
	}
	holder, err := cleanText("body.holder_name", in.HolderName, 2, 300)
	if err != nil {
		return CollectionAccount{}, err
	}
	bank, err := cleanText("body.bank_name", in.BankName, 2, 120)
	if err != nil {
		return CollectionAccount{}, err
	}
	if err := s.checkPlatformParty(ctx, in.LegalPartyID); err != nil {
		return CollectionAccount{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.PaymentsCollectionAccountCreate, authz.TypeCollectionAccount, nil, nil, nil))
	if err != nil {
		return CollectionAccount{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op                        string
		Label, IBAN, Holder, Bank string
		Party                     uuid.UUID
	}{"create_collection_account", label, number, holder, bank, in.LegalPartyID})
	if err != nil {
		return CollectionAccount{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		stored, err := s.d.Store.InsertAccount(ctx, CollectionAccount{
			ID: uuid.NewV7(), Label: label, LegalPartyID: in.LegalPartyID, PartyKind: PartyKindPlatform, IBAN: number,
			HolderName: holder, BankName: bank, Status: AccountPending, CreatedBy: a.UserID,
		})
		if err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.d.Audit.Log(ctx, accountEvent(a, authz.PaymentsCollectionAccountCreate, stored,
			map[string]any{"bank_code": number[4:9], audit.MetaReauth: kind}))
	})
	if err != nil {
		return CollectionAccount{}, err
	}
	return s.d.Store.GetAccount(ctx, id, false)
}

// checkPlatformParty accepts Koro's own legal party only (02 §20.1:
// legal_party_id → kind 'platform').
func (s *service) checkPlatformParty(ctx context.Context, id uuid.UUID) error {
	if s.d.Parties == nil {
		return ErrUnavailable
	}
	bad := fieldErr("body.legal_party_id", "must be Koro's own legal party (kind platform)")
	if id == uuid.Nil() {
		return bad
	}
	p, err := s.d.Parties.Platform(ctx)
	switch {
	case errors.Is(err, parties.ErrNotFound):
		return bad
	case err != nil:
		return err
	case p.ID != id:
		return bad
	}
	return nil
}

// DecideCollectionAccount implements Service, the second eye (03 §6.1,
// pad·4·R, fin·4·R): the gate (403 before reading: the deciders are the
// list's cells) → the decision (422) → the account (404) → the eyes (403
// authz.four_eyes_violation: never its creator) → the re-authentication
// proof (403) → in one transaction: pending_approval only (409
// payments.invalid_state) → active or rejected, decided by and at → audit
// → collection_account.approved (O: the security notice to every platform
// and finance admin). Approval does not make it the default.
func (s *service) DecideCollectionAccount(ctx context.Context, a authz.Actor, id uuid.UUID, d Decision, proof identity.ReauthProof) (CollectionAccount, error) {
	if err := authz.Can(ctx, a, authz.PaymentsCollectionAccountList, adminSurface); err != nil {
		return CollectionAccount{}, err
	}
	if !d.Valid() {
		return CollectionAccount{}, fieldErr("body.decision", "must be approved or rejected")
	}
	c, err := s.d.Store.GetAccount(ctx, id, false)
	if err != nil {
		return CollectionAccount{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsCollectionAccountApprove, accountResource(c)); err != nil {
		return CollectionAccount{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.PaymentsCollectionAccountApprove, authz.TypeCollectionAccount, &c.ID, nil,
		map[string]any{"decision": string(d)}))
	if err != nil {
		return CollectionAccount{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Account  uuid.UUID
		Decision Decision
	}{"decide_collection_account", id, d})
	if err != nil {
		return CollectionAccount{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetAccount(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != AccountPending {
			return uuid.Nil(), ErrInvalidState
		}
		if err := authz.Can(ctx, a, authz.PaymentsCollectionAccountApprove, accountResource(cur)); err != nil {
			return uuid.Nil(), err
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.ApprovedBy, next.ApprovedAt = AccountActive, &by, &now
		if d == DecisionRejected {
			next.Status = AccountRejected
		}
		stored, err := s.d.Store.UpdateAccount(ctx, next, AccountPending)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, accountEvent(a, authz.PaymentsCollectionAccountApprove, stored,
			map[string]any{"decision": string(d), "created_by": cur.CreatedBy.String(), audit.MetaReauth: kind})); err != nil {
			return uuid.Nil(), err
		}
		if d == DecisionApproved {
			return stored.ID, s.emit(ctx, aggregateAccount, stored.ID, domainevent.CollectionAccountApproved{CollectionAccountID: stored.ID})
		}
		return stored.ID, nil
	})
	if err != nil {
		return CollectionAccount{}, err
	}
	return s.d.Store.GetAccount(ctx, id, false)
}

// RetireCollectionAccount implements Service (pad·R, fin·R): authz (403) →
// the account (404) → the re-authentication proof (403) → in one
// transaction: active only (409) → retired (no longer the default: orders
// answer 409 payments.no_active_collection_account until another account
// is made the default) → audit → the security notice (A). Transfers that
// still arrive at a retired account can be recorded.
func (s *service) RetireCollectionAccount(ctx context.Context, a authz.Actor, id uuid.UUID, proof identity.ReauthProof) (CollectionAccount, error) {
	return s.changeAccount(ctx, a, id, proof, changeRetired, func(ctx context.Context, cur CollectionAccount) (CollectionAccount, map[string]any, error) {
		now := s.now()
		next := cur
		next.Status, next.RetiredAt, next.IsDefault = AccountRetired, &now, false
		stored, err := s.d.Store.UpdateAccount(ctx, next, AccountActive)
		return stored, map[string]any{"was_default": cur.IsDefault}, err
	})
}

// MakeDefaultCollectionAccount implements Service (pad·R, fin·R; action
// payments.collection_account.retire, 04 §2.16): authz (403) → the account
// (404) → the re-authentication proof (403) → in one transaction: active
// only (409) → the previous default steps down, this one becomes the
// account of every new payment instruction (hooks.collectionAccount) →
// audit → the security notice (A). Already the default: no change.
func (s *service) MakeDefaultCollectionAccount(ctx context.Context, a authz.Actor, id uuid.UUID, proof identity.ReauthProof) (CollectionAccount, error) {
	return s.changeAccount(ctx, a, id, proof, changeMadeDefault, func(ctx context.Context, cur CollectionAccount) (CollectionAccount, map[string]any, error) {
		if cur.IsDefault {
			return cur, nil, nil
		}
		meta := map[string]any{}
		old, found, err := s.d.Store.DefaultAccount(ctx)
		if err != nil {
			return CollectionAccount{}, nil, err
		}
		if found {
			prev := old
			prev.IsDefault = false
			if _, err := s.d.Store.UpdateAccount(ctx, prev, AccountActive); err != nil {
				return CollectionAccount{}, nil, err
			}
			meta["previous_default_id"] = old.ID.String()
		}
		next := cur
		next.IsDefault = true
		stored, err := s.d.Store.UpdateAccount(ctx, next, AccountActive)
		return stored, meta, err
	})
}

// changeAccount is the shared flow of retire and make-default; apply runs in
// the transaction on the locked, active account and returns the audit
// metadata (nil: nothing changed, nothing is audited or announced).
func (s *service) changeAccount(ctx context.Context, a authz.Actor, id uuid.UUID, proof identity.ReauthProof, change string,
	apply func(ctx context.Context, cur CollectionAccount) (CollectionAccount, map[string]any, error)) (CollectionAccount, error) {
	if err := authz.Can(ctx, a, authz.PaymentsCollectionAccountRetire, adminSurface); err != nil {
		return CollectionAccount{}, err
	}
	if _, err := s.d.Store.GetAccount(ctx, id, false); err != nil {
		return CollectionAccount{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.PaymentsCollectionAccountRetire, authz.TypeCollectionAccount, &id, nil,
		map[string]any{"change": change}))
	if err != nil {
		return CollectionAccount{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Account uuid.UUID
	}{change + "_collection_account", id})
	if err != nil {
		return CollectionAccount{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetAccount(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != AccountActive {
			return uuid.Nil(), ErrInvalidState
		}
		stored, meta, err := apply(ctx, cur)
		if err != nil || meta == nil {
			return cur.ID, err
		}
		meta["change"], meta[audit.MetaReauth] = change, kind
		if err := s.d.Audit.Log(ctx, accountEvent(a, authz.PaymentsCollectionAccountRetire, stored, meta)); err != nil {
			return uuid.Nil(), err
		}
		s.d.Queue.AfterCommit(ctx, NotifyAccountChangedTaskName, accountNotice{CollectionAccountID: stored.ID, Change: change},
			NotifyAccountChangedTaskName+":"+stored.ID.String()+":"+change+":"+s.now().Format("20060102T150405.000000"))
		s.logInfo(ctx, "collection account changed", slog.String("collection_account_id", stored.ID.String()),
			slog.String("change", change))
		return stored.ID, nil
	})
	if err != nil {
		return CollectionAccount{}, err
	}
	return s.d.Store.GetAccount(ctx, id, false)
}
