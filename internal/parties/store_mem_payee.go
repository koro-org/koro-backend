package parties

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/iban"
	"koro/internal/platform/problem"
)

// checkPayee applies the CHECK constraints of 000096 (02 §17.3).
func checkPayee(p Payee) error {
	switch {
	case p.PartyKind != KindPayee, !p.Status.Valid(), !p.PayoutMethod.Valid(), !p.InvoicingCapability.Valid():
		return errCheck
	case runeLen(p.UniversityProtocolRef) > maxProtocolRef:
		return errCheck
	case (p.VerifiedAt == nil) != (p.VerifiedBy == nil): // payees_verified_pair_chk
		return errCheck
	case p.Status == PayeeActive && p.VerifiedAt == nil: // payees_active_chk
		return errCheck
	case p.ValidTo != nil && !p.ValidTo.After(p.ValidFrom): // payees_validity_chk
		return errCheck
	case p.PayoutMethod == PayoutIyzico: // payees_iyzico_chk: no sub-merchant key in v1
		return errCheck
	}
	return nil
}

// uniquePayee applies payees_org_party_uq and payees_default_uq.
func (s *MemStore) uniquePayee(p Payee) error {
	for _, o := range s.payees {
		if o.ID == p.ID {
			continue
		}
		if o.OrgID == p.OrgID && o.LegalPartyID == p.LegalPartyID && o.Status != PayeeClosed && p.Status != PayeeClosed {
			return ErrPayeeExists
		}
		if o.OrgID == p.OrgID && o.IsDefault && o.Status == PayeeActive && p.IsDefault && p.Status == PayeeActive {
			return problem.ErrConflict
		}
	}
	return nil
}

// InsertPayee implements Store.
func (s *MemStore) InsertPayee(_ context.Context, p Payee) (Payee, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.PartyKind = KindPayee
	if party, ok := s.parties[p.LegalPartyID]; !ok || party.Kind != KindPayee { // payees_legal_party_id_party_kind_fkey
		return Payee{}, problem.ErrConflict
	}
	if _, dup := s.payees[p.ID]; dup || p.CreatedBy == uuid.Nil() {
		return Payee{}, problem.ErrConflict
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if p.ValidFrom.IsZero() {
		p.ValidFrom = now
	}
	if err := checkPayee(p); err != nil {
		return Payee{}, err
	}
	if err := s.uniquePayee(p); err != nil {
		return Payee{}, err
	}
	p.CreatedAt, p.UpdatedAt = now, now
	s.payees[p.ID] = p
	return p, nil
}

// GetPayee implements Store (no locks in memory).
func (s *MemStore) GetPayee(_ context.Context, id uuid.UUID, _ bool) (Payee, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payees[id]
	if !ok {
		return Payee{}, ErrNotFound
	}
	return p, nil
}

// OrgPayee implements Store.
func (s *MemStore) OrgPayee(ctx context.Context, orgID, id uuid.UUID, lock bool) (Payee, error) {
	if orgID == uuid.Nil() {
		return Payee{}, db.ErrMissingOrgScope
	}
	p, err := s.GetPayee(ctx, id, lock)
	if err != nil {
		return Payee{}, err
	}
	if p.OrgID != orgID {
		return Payee{}, ErrNotFound
	}
	return p, nil
}

func byID[T any](id func(T) uuid.UUID) func(a, b T) int {
	return func(a, b T) int { return cmp.Compare(id(a).String(), id(b).String()) }
}

func payeeID(p Payee) uuid.UUID             { return p.ID }
func accountID(b BankAccount) uuid.UUID     { return b.ID }
func accountNewest(a, b BankAccount) int    { return cmp.Compare(b.ID.String(), a.ID.String()) }
func payeeOf(orgID uuid.UUID, p Payee) bool { return p.OrgID == orgID }

// OrgPayees implements Store.
func (s *MemStore) OrgPayees(_ context.Context, orgID uuid.UUID) ([]Payee, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Payee
	for _, p := range s.payees {
		if payeeOf(orgID, p) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, byID(payeeID))
	return out, nil
}

// ListPayees implements Store.
func (s *MemStore) ListPayees(_ context.Context, f PayeeFilter, after *uuid.UUID, limit int) ([]Payee, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Payee
	for _, p := range s.payees {
		lp := s.parties[p.LegalPartyID]
		switch {
		case f.OrgID != nil && p.OrgID != *f.OrgID:
		case f.Status != "" && p.Status != f.Status:
		case after != nil && p.ID.String() >= after.String():
		case f.Q != "" && isVKNQuery(f.Q) && (lp.TaxNo == nil || *lp.TaxNo != f.Q):
		case f.Q != "" && !isVKNQuery(f.Q) && !strings.Contains(memNorm(lp.Title), memNorm(f.Q)):
		default:
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Payee) int { return cmp.Compare(b.ID.String(), a.ID.String()) }) // newest first
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DefaultPayee implements Store.
func (s *MemStore) DefaultPayee(_ context.Context, orgID uuid.UUID) (Payee, bool, error) {
	if orgID == uuid.Nil() {
		return Payee{}, false, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.payees {
		if p.OrgID == orgID && p.IsDefault && p.Status == PayeeActive {
			return p, true, nil
		}
	}
	return Payee{}, false, nil
}

// UpdatePayee implements Store: only the mutable columns change.
func (s *MemStore) UpdatePayee(_ context.Context, p Payee) (Payee, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.payees[p.ID]
	if !ok {
		return Payee{}, ErrNotFound
	}
	next := cur
	next.Status, next.IsDefault, next.PayoutMethod, next.InvoicingCapability = p.Status, p.IsDefault, p.PayoutMethod, p.InvoicingCapability
	next.UniversityProtocolRef, next.VerifiedAt, next.VerifiedBy, next.ValidTo = p.UniversityProtocolRef, p.VerifiedAt, p.VerifiedBy, p.ValidTo
	if err := checkPayee(next); err != nil {
		return Payee{}, err
	}
	if err := s.uniquePayee(next); err != nil {
		return Payee{}, err
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.payees[next.ID] = next
	return next, nil
}

// ClearDefault implements Store.
func (s *MemStore) ClearDefault(_ context.Context, orgID, keep uuid.UUID) ([]uuid.UUID, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []uuid.UUID
	for id, p := range s.payees {
		if p.OrgID == orgID && p.IsDefault && p.Status == PayeeActive && id != keep {
			p.IsDefault, p.UpdatedAt = false, s.stamp(p.UpdatedAt)
			s.payees[id] = p
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return cmp.Compare(a.String(), b.String()) })
	return ids, nil
}

// PayeesCreatedBy implements Store.
func (s *MemStore) PayeesCreatedBy(_ context.Context, userID uuid.UUID) ([]Payee, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Payee
	for _, p := range s.payees {
		if p.CreatedBy == userID {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, byID(payeeID))
	return out, nil
}

// errFourEyesCheck mirrors the four-eyes CHECKs of 000097 as the GORM store
// translates them (payeeConstraints).
var errFourEyesCheck = authz.ErrFourEyes

func optLen(p *string) int { return runeLen(p) }

// checkAccount applies the CHECK constraints of 000097 (02 §17.4, 06 H-37).
func checkAccount(b BankAccount) error {
	st := b.Status
	approvedLike := st == AccountApproved || st == AccountSuperseded || st == AccountRevoked
	switch {
	case !iban.Valid(b.IBAN):
		return ErrIBANInvalid
	case !st.Valid():
		return errCheck
	case utf8.RuneCountInString(b.HolderName) < minTitle || utf8.RuneCountInString(b.HolderName) > maxTitle:
		return errCheck
	case optLen(b.BankName) > maxBankName, optLen(b.RejectionReason) > maxNote, optLen(b.RevokeReason) > maxNote:
		return errCheck
	// four eyes (03 §6.1)
	case b.ApprovedBy != nil && *b.ApprovedBy == b.SubmittedBy,
		b.ApprovedBy != nil && b.VerifiedBy != nil && *b.ApprovedBy == *b.VerifiedBy,
		b.VerifiedBy != nil && *b.VerifiedBy == b.SubmittedBy:
		return errFourEyesCheck
	case !b.SubmittedByAdmin && st != AccountPendingVerification && st != AccountRejected && b.VerifiedBy == nil: // org_flow
		return errCheck
	case b.SubmittedByAdmin && (st == AccountPendingVerification || b.VerifiedBy != nil): // admin_flow
		return errCheck
	// one CHECK per state (06 H-37)
	case (b.VerifiedBy == nil) != (b.VerifiedAt == nil), st == AccountPendingVerification && b.VerifiedBy != nil:
		return errCheck
	case approvedLike != (b.ApprovedBy != nil), approvedLike != (b.ApprovedAt != nil):
		return errCheck
	case (st == AccountRejected) != (b.RejectedBy != nil), (st == AccountRejected) != (b.RejectedAt != nil),
		(st == AccountRejected) != (b.RejectionReason != nil):
		return errCheck
	case (st == AccountSuperseded) != (b.SupersededAt != nil), (st == AccountSuperseded) != (b.SupersededByID != nil),
		b.SupersededByID != nil && *b.SupersededByID == b.ID:
		return errCheck
	case (st == AccountRevoked) != (b.RevokedBy != nil), (st == AccountRevoked) != (b.RevokedAt != nil),
		(st == AccountRevoked) != (b.RevokeReason != nil):
		return errCheck
	}
	return nil
}

// uniqueAccount applies payee_bank_accounts_approved_uq and _pending_uq and
// the foreign keys of the row (payee with its org, successor of the same
// payee).
func (s *MemStore) uniqueAccount(b BankAccount) error {
	p, ok := s.payees[b.PayeeID]
	if !ok || p.OrgID != b.OrgID { // payee_bank_accounts_payee_id_org_id_fkey
		return problem.ErrConflict
	}
	if b.SupersededByID != nil {
		if next, ok := s.accounts[*b.SupersededByID]; !ok || next.PayeeID != b.PayeeID {
			return problem.ErrConflict
		}
	}
	for _, o := range s.accounts {
		if o.ID == b.ID || o.PayeeID != b.PayeeID {
			continue
		}
		switch {
		case o.Status == AccountApproved && b.Status == AccountApproved:
			return problem.ErrConflict
		case o.Status.Pending() && b.Status.Pending():
			return ErrBankAccountPending
		}
	}
	return nil
}

// InsertBankAccount implements Store (with payee_bank_accounts_initial_status).
func (s *MemStore) InsertBankAccount(_ context.Context, b BankAccount) (BankAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !b.Status.Pending() {
		return BankAccount{}, errCheck
	}
	if _, dup := s.accounts[b.ID]; dup {
		return BankAccount{}, problem.ErrConflict
	}
	if err := checkAccount(b); err != nil {
		return BankAccount{}, err
	}
	if err := s.uniqueAccount(b); err != nil {
		return BankAccount{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if b.SubmittedAt.IsZero() {
		b.SubmittedAt = now
	}
	b.CreatedAt, b.UpdatedAt, b.BankCode = now, now, b.IBAN[4:9]
	s.accounts[b.ID] = b
	return b, nil
}

// GetBankAccount implements Store.
func (s *MemStore) GetBankAccount(_ context.Context, id uuid.UUID, _ bool) (BankAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.accounts[id]
	if !ok {
		return BankAccount{}, ErrNotFound
	}
	return b, nil
}

// PayeeBankAccounts implements Store.
func (s *MemStore) PayeeBankAccounts(_ context.Context, payeeID uuid.UUID) ([]BankAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []BankAccount
	for _, b := range s.accounts {
		if b.PayeeID == payeeID {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, accountNewest)
	return out, nil
}

// ApprovedAccounts implements Store.
func (s *MemStore) ApprovedAccounts(_ context.Context, payeeIDs []uuid.UUID, _ bool) (map[uuid.UUID]BankAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]BankAccount{}
	for _, b := range s.accounts {
		if b.Status == AccountApproved && slices.Contains(payeeIDs, b.PayeeID) {
			out[b.PayeeID] = b
		}
	}
	return out, nil
}

// ListBankAccounts implements Store.
func (s *MemStore) ListBankAccounts(_ context.Context, statuses []BankAccountStatus, after *uuid.UUID, limit int) ([]BankAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []BankAccount
	for _, b := range s.accounts {
		switch {
		case len(statuses) > 0 && !slices.Contains(statuses, b.Status):
		case after != nil && b.ID.String() <= after.String():
		default:
			out = append(out, b)
		}
	}
	slices.SortFunc(out, byID(accountID))
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// TransitionBankAccount implements Store: only the state columns change
// (payee_bank_accounts_frozen keeps the rest).
func (s *MemStore) TransitionBankAccount(_ context.Context, b BankAccount, from BankAccountStatus) (BankAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.accounts[b.ID]
	if !ok {
		return BankAccount{}, ErrNotFound
	}
	if cur.Status != from {
		return BankAccount{}, ErrInvalidState
	}
	next := cur
	next.Status, next.VerifiedBy, next.VerifiedAt = b.Status, b.VerifiedBy, b.VerifiedAt
	next.ApprovedBy, next.ApprovedAt = b.ApprovedBy, b.ApprovedAt
	next.RejectedBy, next.RejectedAt, next.RejectionReason = b.RejectedBy, b.RejectedAt, b.RejectionReason
	next.SupersededAt, next.SupersededByID = b.SupersededAt, b.SupersededByID
	next.RevokedBy, next.RevokedAt, next.RevokeReason = b.RevokedBy, b.RevokedAt, b.RevokeReason
	if err := checkAccount(next); err != nil {
		return BankAccount{}, err
	}
	if err := s.uniqueAccount(next); err != nil {
		return BankAccount{}, err
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.accounts[next.ID] = next
	return next, nil
}

// BankAccountsSubmittedBy implements Store.
func (s *MemStore) BankAccountsSubmittedBy(_ context.Context, userID uuid.UUID) ([]BankAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []BankAccount
	for _, b := range s.accounts {
		if b.SubmittedBy == userID {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, byID(accountID))
	return out, nil
}
