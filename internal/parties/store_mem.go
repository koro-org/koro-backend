package parties

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/files"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// errCheck mirrors a CHECK violation of 02 §17 (the GORM store gets 422
// from db.Translate).
var errCheck = problem.ErrValidation.Wrap(errors.New("parties: check constraint violated"))

var (
	memVKNRe    = regexp.MustCompile(`^[0-9]{10}$`)
	memMERSISRe = regexp.MustCompile(`^[0-9]{16}$`)
)

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It follows the GORM store's contract, including the CHECK,
// unique and foreign key constraints of 02 §17.1–17.4 (000094–000097) a test could
// otherwise only meet in PostgreSQL. Every method is atomic under one
// mutex; it has no transactions (db.NopTx).
type MemStore struct {
	mu       sync.Mutex
	parties  map[uuid.UUID]LegalParty
	docs     map[uuid.UUID]Document
	payees   map[uuid.UUID]Payee
	accounts map[uuid.UUID]BankAccount
	// now stamps created_at and updated_at like the defaults and the
	// *_touch_updated_at triggers.
	now func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory Store; now stamps the rows.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{parties: map[uuid.UUID]LegalParty{}, docs: map[uuid.UUID]Document{}, payees: map[uuid.UUID]Payee{},
		accounts: map[uuid.UUID]BankAccount{}, now: now}
}

// LinkPayee records a payees row (id, seller org, legal party, status) with
// the column defaults (bank_eft, not default; an active one verified by
// its own id), without the foreign key checks of InsertPayee. It is a seed
// shortcut of service and matrix tests; status is a payees.status value
// ("closed" rows relate nothing).
func (s *MemStore) LinkPayee(id, orgID, partyID uuid.UUID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC().Truncate(time.Microsecond)
	p := Payee{ID: id, LegalPartyID: partyID, PartyKind: KindPayee, OrgID: orgID, Status: PayeeStatus(status),
		PayoutMethod: PayoutBankEFT, InvoicingCapability: InvoicingUnknown, ValidFrom: now, CreatedBy: id,
		CreatedAt: now, UpdatedAt: now}
	if p.Status == PayeeActive {
		p.VerifiedAt, p.VerifiedBy = &now, &id
	}
	s.payees[id] = p
}

func (s *MemStore) stamp(prev time.Time) time.Time {
	t := s.now().UTC().Truncate(time.Microsecond)
	if !t.After(prev) {
		t = prev.Add(time.Microsecond)
	}
	return t
}

func runeLen(p *string) int {
	if p == nil {
		return 0
	}
	return utf8.RuneCountInString(*p)
}

// checkParty applies the CHECK constraints of 02 §17.1 (000094).
func checkParty(p LegalParty) error {
	individual := p.LegalType == LegalIndividual
	switch {
	case !p.Kind.Valid(), !p.LegalType.Valid(), !p.Status.Valid(), !p.VATStatus.Valid():
		return errCheck
	case utf8.RuneCountInString(p.Title) < minTitle || utf8.RuneCountInString(p.Title) > maxTitle:
		return errCheck
	case runeLen(p.TaxOffice) > maxTaxOffice, runeLen(p.Email) > maxEmail:
		return errCheck
	case p.TaxNo != nil && !memVKNRe.MatchString(*p.TaxNo):
		return errCheck
	case p.MersisNo != nil && !memMERSISRe.MatchString(*p.MersisNo):
		return errCheck
	case p.Phone != nil && !phoneRe.MatchString(*p.Phone):
		return errCheck
	case p.TCKNBidx != nil && len(p.TCKNBidx) != 32:
		return errCheck
	case individual != (p.TCKNEnc != nil), individual != (p.TCKNBidx != nil): // legal_parties_tckn_chk
		return errCheck
	case p.Status == StatusVerified && !individual && p.TaxNo == nil: // legal_parties_verified_tax_no_chk
		return errCheck
	case (p.Status == StatusVerified) != (p.VerifiedAt != nil), (p.Status == StatusVerified) != (p.VerifiedBy != nil):
		return errCheck
	case p.Kind == KindBuyerCompany && p.OrgID == nil: // legal_parties_buyer_org_chk
		return errCheck
	}
	return nil
}

// uniqueParty applies legal_parties_platform_uq, _taxno_uq and _tckn_uq
// against every other row.
func (s *MemStore) uniqueParty(p LegalParty) error {
	for _, o := range s.parties {
		if o.ID == p.ID {
			continue
		}
		switch {
		case p.Kind == KindPlatform && o.Kind == KindPlatform:
			return ErrPlatformExists
		case o.Kind != p.Kind || o.Status == StatusInactive || p.Status == StatusInactive:
		case p.TaxNo != nil && o.TaxNo != nil && *p.TaxNo == *o.TaxNo:
			return ErrTaxIDTaken
		case p.TCKNBidx != nil && bytes.Equal(p.TCKNBidx, o.TCKNBidx):
			return ErrTaxIDTaken
		}
	}
	return nil
}

func cloneParty(p LegalParty) LegalParty {
	p.TCKNEnc, p.TCKNBidx = bytes.Clone(p.TCKNEnc), bytes.Clone(p.TCKNBidx)
	return p
}

// InsertParty implements Store.
func (s *MemStore) InsertParty(_ context.Context, p LegalParty) (LegalParty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertParty(p)
}

func (s *MemStore) insertParty(p LegalParty) (LegalParty, error) {
	if err := checkParty(p); err != nil {
		return LegalParty{}, err
	}
	if _, dup := s.parties[p.ID]; dup {
		return LegalParty{}, problem.ErrConflict
	}
	if err := s.uniqueParty(p); err != nil {
		return LegalParty{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
	p = cloneParty(p)
	s.parties[p.ID] = p
	return cloneParty(p), nil
}

// InsertPlatformIfAbsent implements Store.
func (s *MemStore) InsertPlatformIfAbsent(_ context.Context, p LegalParty) (LegalParty, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.parties {
		if o.Kind == KindPlatform {
			return cloneParty(o), false, nil
		}
	}
	if p.Kind != KindPlatform {
		return LegalParty{}, false, errCheck
	}
	stored, err := s.insertParty(p)
	if err != nil {
		return LegalParty{}, false, err
	}
	return stored, true, nil
}

// GetParty implements Store.
func (s *MemStore) GetParty(_ context.Context, id uuid.UUID) (LegalParty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.parties[id]
	if !ok {
		return LegalParty{}, ErrNotFound
	}
	return cloneParty(p), nil
}

// LockParty implements Store (no locks in memory).
func (s *MemStore) LockParty(ctx context.Context, id uuid.UUID) (LegalParty, error) {
	return s.GetParty(ctx, id)
}

// GetPlatform implements Store.
func (s *MemStore) GetPlatform(_ context.Context) (LegalParty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.parties {
		if p.Kind == KindPlatform {
			return cloneParty(p), nil
		}
	}
	return LegalParty{}, ErrNotFound
}

// UpdateParty implements Store: kind, org_id, created_by and created_at
// keep their stored values.
func (s *MemStore) UpdateParty(_ context.Context, p LegalParty) (LegalParty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.parties[p.ID]
	if !ok {
		return LegalParty{}, ErrNotFound
	}
	next := cloneParty(p)
	next.Kind, next.OrgID, next.CreatedBy, next.CreatedAt = cur.Kind, cur.OrgID, cur.CreatedBy, cur.CreatedAt
	if err := checkParty(next); err != nil {
		return LegalParty{}, err
	}
	if err := s.uniqueParty(next); err != nil {
		return LegalParty{}, err
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.parties[next.ID] = next
	return cloneParty(next), nil
}

// scopeOf returns how party p relates to orgID ("" when it does not).
func (s *MemStore) scopeOf(orgID uuid.UUID, p LegalParty) Scope {
	if p.Kind == KindPlatform {
		return ""
	}
	for _, py := range s.payees {
		if py.OrgID == orgID && py.LegalPartyID == p.ID && py.Status != PayeeClosed {
			return ScopePayee
		}
	}
	if p.OrgID != nil && *p.OrgID == orgID {
		return ScopeOrg
	}
	return ""
}

// OrgParties implements Store.
func (s *MemStore) OrgParties(_ context.Context, orgID uuid.UUID) ([]OrgParty, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []OrgParty
	for _, p := range s.parties {
		if sc := s.scopeOf(orgID, p); sc != "" {
			out = append(out, OrgParty{Party: cloneParty(p), Scope: sc})
		}
	}
	slices.SortFunc(out, func(a, b OrgParty) int { return cmp.Compare(a.Party.ID.String(), b.Party.ID.String()) })
	return out, nil
}

// OrgParty implements Store.
func (s *MemStore) OrgParty(_ context.Context, orgID, id uuid.UUID, _ bool) (OrgParty, error) {
	if orgID == uuid.Nil() {
		return OrgParty{}, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.parties[id]
	if !ok {
		return OrgParty{}, ErrNotFound
	}
	sc := s.scopeOf(orgID, p)
	if sc == "" {
		return OrgParty{}, ErrNotFound
	}
	return OrgParty{Party: cloneParty(p), Scope: sc}, nil
}

// newestFirst orders parties by descending id (UUIDv7: creation order).
func newestFirst(a, b LegalParty) int { return cmp.Compare(b.ID.String(), a.ID.String()) }

// BuyerParties implements Store.
func (s *MemStore) BuyerParties(_ context.Context, orgID uuid.UUID) ([]LegalParty, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LegalParty
	for _, p := range s.parties {
		if p.Kind == KindBuyerCompany && p.OrgID != nil && *p.OrgID == orgID {
			out = append(out, cloneParty(p))
		}
	}
	slices.SortFunc(out, newestFirst)
	return out, nil
}

// memNorm approximates koro_norm (Turkish lower case, accents removed) for
// the in-memory title search.
func memNorm(s string) string { return textx.Slug(s, 0) }

// ListParties implements Store.
func (s *MemStore) ListParties(_ context.Context, f ListFilter, after *uuid.UUID, limit int) ([]LegalParty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LegalParty
	for _, p := range s.parties {
		switch {
		case f.Kind != "" && p.Kind != f.Kind:
		case f.Status != "" && p.Status != f.Status:
		case after != nil && p.ID.String() >= after.String():
		case f.Q != "" && isVKNQuery(f.Q) && (p.TaxNo == nil || *p.TaxNo != f.Q):
		case f.Q != "" && !isVKNQuery(f.Q) && !strings.Contains(memNorm(p.Title), memNorm(f.Q)):
		default:
			out = append(out, cloneParty(p))
		}
	}
	slices.SortFunc(out, newestFirst)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PartiesCreatedBy implements Store.
func (s *MemStore) PartiesCreatedBy(_ context.Context, userID uuid.UUID) ([]LegalParty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LegalParty
	for _, p := range s.parties {
		if p.CreatedBy != nil && *p.CreatedBy == userID {
			out = append(out, cloneParty(p))
		}
	}
	slices.SortFunc(out, func(a, b LegalParty) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return out, nil
}

// checkDocument applies the constraints of 02 §17.2 (000095).
func (s *MemStore) checkDocument(d Document) error {
	if _, ok := s.parties[d.LegalPartyID]; !ok { // legal_party_documents_legal_party_id_fkey
		return problem.ErrConflict
	}
	if !d.Kind.Valid() || (d.VerifiedAt == nil) != (d.VerifiedBy == nil) || d.FileID == uuid.Nil() || d.UploadedBy == uuid.Nil() {
		return errCheck
	}
	return nil
}

// InsertDocument implements Store.
func (s *MemStore) InsertDocument(_ context.Context, d Document) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkDocument(d); err != nil {
		return Document{}, err
	}
	if _, dup := s.docs[d.ID]; dup {
		return Document{}, problem.ErrConflict
	}
	for _, o := range s.docs {
		if o.FileID == d.FileID { // legal_party_documents_file_id_uq
			return Document{}, files.ErrAlreadyAttached
		}
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = s.now().UTC().Truncate(time.Microsecond)
	}
	s.docs[d.ID] = d
	return d, nil
}

// GetDocument implements Store.
func (s *MemStore) GetDocument(_ context.Context, id uuid.UUID, _ bool) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[id]
	if !ok {
		return Document{}, ErrNotFound
	}
	return d, nil
}

// PartyDocuments implements Store.
func (s *MemStore) PartyDocuments(_ context.Context, partyIDs []uuid.UUID) ([]Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Document
	for _, d := range s.docs {
		if slices.Contains(partyIDs, d.LegalPartyID) {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b Document) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return out, nil
}

// SetDocumentVerified implements Store.
func (s *MemStore) SetDocumentVerified(_ context.Context, id, by uuid.UUID, at time.Time) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[id]
	if !ok {
		return Document{}, ErrNotFound
	}
	d.VerifiedAt, d.VerifiedBy = &at, &by
	if err := s.checkDocument(d); err != nil {
		return Document{}, err
	}
	s.docs[id] = d
	return d, nil
}

// DocumentsUploadedBy implements Store.
func (s *MemStore) DocumentsUploadedBy(_ context.Context, userID uuid.UUID) ([]Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Document
	for _, d := range s.docs {
		if d.UploadedBy == userID {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b Document) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return out, nil
}
