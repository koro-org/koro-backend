package parties_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/files"
	"koro/internal/parties"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// world creates the parent rows a store needs: orgs, users, party_document
// files and payees rows (FKs in PostgreSQL; plain IDs and LinkPayee in
// memory).
type world struct {
	org   func(t *testing.T) uuid.UUID
	user  func(t *testing.T) uuid.UUID
	file  func(t *testing.T) uuid.UUID
	payee func(t *testing.T, org, party uuid.UUID, status string)
}

func memWorld(s *parties.MemStore) world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	return world{org: id, user: id, file: id, payee: func(_ *testing.T, org, party uuid.UUID, status string) {
		s.LinkPayee(uuid.NewV7(), org, party, status)
	}}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (parties.Store, world) {
		s := parties.NewMemStore(func() time.Time { return t0 })
		return s, memWorld(s)
	})
}

// TestMemStore_SeedAt pins the fixture shortcut: rows written inside are
// stamped at the pinned time by the store itself, the store's clock is
// back afterwards, and a caller's timestamp is still ignored (BE-064).
func TestMemStore_SeedAt(t *testing.T) {
	t.Parallel()
	s := parties.NewMemStore(func() time.Time { return t0 })
	at := t0.Add(-72 * time.Hour)
	vkn := vknA
	var seeded parties.LegalParty
	err := s.SeedAt(at, func() error {
		var err error
		seeded, err = s.InsertParty(t.Context(), parties.LegalParty{ID: uuid.NewV7(), Kind: parties.KindPayee,
			LegalType: parties.LegalFoundation, Title: "Örnek Vakfı", TaxNo: &vkn, VATStatus: parties.VATUnknown,
			Status: parties.StatusPendingVerification, CreatedAt: t0, UpdatedAt: t0})
		return err
	})
	if err != nil || !seeded.CreatedAt.Equal(at) || !seeded.UpdatedAt.Equal(at) {
		t.Fatalf("SeedAt insert = %+v, %v; want stamped at %v", seeded, err, at)
	}
	seeded.Title = "Yeni Vakıf"
	upd, err := s.UpdateParty(t.Context(), seeded)
	if err != nil || !upd.UpdatedAt.Equal(t0) || !upd.CreatedAt.Equal(at) {
		t.Fatalf("update after SeedAt = %+v, %v; want updated_at %v", upd, err, t0)
	}
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (parties.Store, world)) {
	t.Helper()
	stale := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC) // a caller's timestamp the stores ignore
	party := func(kind parties.Kind, lt parties.LegalType, org *uuid.UUID, vkn string) parties.LegalParty {
		p := parties.LegalParty{
			ID: uuid.NewV7(), Kind: kind, LegalType: lt, OrgID: org, Title: "Örnek Ünvan A.Ş.", VATStatus: parties.VATUnknown,
			Status: parties.StatusPendingVerification, Address: parties.Address{Line1: "Cad. 1", City: "İzmir", Country: "TR"},
		}
		if vkn != "" {
			p.TaxNo = &vkn
		}
		return p
	}
	insert := func(t *testing.T, s parties.Store, p parties.LegalParty) parties.LegalParty {
		t.Helper()
		got, err := s.InsertParty(t.Context(), p)
		if err != nil {
			t.Fatalf("InsertParty: %v", err)
		}
		return got
	}

	t.Run("insert, get, update and constraints", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		co, by := w.org(t), w.user(t)
		p := party(parties.KindBuyerCompany, parties.LegalCompany, &co, vknA)
		p.CreatedBy = &by
		// The store owns both timestamps (one clock, BE-064): a caller's value is ignored.
		p.CreatedAt, p.UpdatedAt = stale, stale
		got := insert(t, s, p)
		if got.ID != p.ID || got.Title != p.Title || got.Address != p.Address || got.OrgID == nil || *got.OrgID != co ||
			got.CreatedAt.IsZero() || got.CreatedAt.Equal(stale) || !got.UpdatedAt.Equal(got.CreatedAt) || *got.TaxNo != vknA {
			t.Fatalf("InsertParty = %+v", got)
		}
		if _, err := s.GetParty(t.Context(), uuid.NewV7()); !errors.Is(err, parties.ErrNotFound) {
			t.Fatalf("GetParty of a missing row: %v", err)
		}
		if _, err := s.LockParty(t.Context(), uuid.NewV7()); !errors.Is(err, parties.ErrNotFound) {
			t.Fatalf("LockParty of a missing row: %v", err)
		}
		// legal_parties_taxno_uq: per kind, active rows only.
		dup := party(parties.KindBuyerCompany, parties.LegalCompany, &co, vknA)
		if _, err := s.InsertParty(t.Context(), dup); !errors.Is(err, parties.ErrTaxIDTaken) {
			t.Fatalf("duplicate VKN: %v", err)
		}
		insert(t, s, party(parties.KindPayee, parties.LegalFoundation, nil, vknA))
		// CHECKs: a buyer without org, a malformed VKN, a verified party without verifier.
		for _, bad := range []func(p *parties.LegalParty){
			func(p *parties.LegalParty) { p.OrgID = nil },
			func(p *parties.LegalParty) { v := "12345"; p.TaxNo = &v },
			func(p *parties.LegalParty) { p.Status = parties.StatusVerified },
			func(p *parties.LegalParty) { p.Title = "x" },
		} {
			b := party(parties.KindBuyerCompany, parties.LegalCompany, &co, vknB)
			bad(&b)
			if _, err := s.InsertParty(t.Context(), b); !errors.Is(err, problem.ErrValidation) {
				t.Fatalf("CHECK violation: %v", err)
			}
		}
		// UpdateParty: kind, org and created_by never change; updated_at moves.
		next := got
		next.Title, next.Kind, next.OrgID, next.CreatedBy = "Yeni Ünvan A.Ş.", parties.KindPayee, nil, nil
		now := time.Now().UTC().Truncate(time.Microsecond)
		next.Status, next.VerifiedAt, next.VerifiedBy = parties.StatusVerified, &now, &by
		upd, err := s.UpdateParty(t.Context(), next)
		if err != nil {
			t.Fatal(err)
		}
		if upd.Title != "Yeni Ünvan A.Ş." || upd.Kind != parties.KindBuyerCompany || upd.OrgID == nil || *upd.OrgID != co ||
			upd.CreatedBy == nil || *upd.CreatedBy != by || upd.Status != parties.StatusVerified || !upd.UpdatedAt.After(got.UpdatedAt) &&
			!upd.UpdatedAt.Equal(got.UpdatedAt) || upd.UpdatedAt.Before(upd.CreatedAt) {
			t.Fatalf("UpdateParty = %+v", upd)
		}
		other := insert(t, s, party(parties.KindBuyerCompany, parties.LegalCompany, &co, vknC))
		other.TaxNo = ptr(vknA)
		if _, err := s.UpdateParty(t.Context(), other); !errors.Is(err, parties.ErrTaxIDTaken) {
			t.Fatalf("update onto a taken VKN: %v", err)
		}
		// Retiring frees the VKN.
		upd.Status, upd.VerifiedAt, upd.VerifiedBy = parties.StatusInactive, nil, nil
		if _, err := s.UpdateParty(t.Context(), upd); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateParty(t.Context(), other); err != nil {
			t.Fatalf("VKN of an inactive party: %v", err)
		}
		missing := party(parties.KindPayee, parties.LegalFoundation, nil, "")
		if _, err := s.UpdateParty(t.Context(), missing); !errors.Is(err, parties.ErrNotFound) {
			t.Fatalf("UpdateParty of a missing row: %v", err)
		}
		mine, err := s.PartiesCreatedBy(t.Context(), by)
		if err != nil || len(mine) != 1 || mine[0].ID != p.ID {
			t.Fatalf("PartiesCreatedBy = %+v %v", mine, err)
		}
	})

	t.Run("individuals: sealed national ID and blind index", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		ind := party(parties.KindPayee, parties.LegalIndividual, nil, "")
		ind.TCKNEnc, ind.TCKNBidx = []byte("sealed-1"), bytes.Repeat([]byte{1}, 32)
		got := insert(t, s, ind)
		if !bytes.Equal(got.TCKNEnc, ind.TCKNEnc) || !bytes.Equal(got.TCKNBidx, ind.TCKNBidx) {
			t.Fatalf("stored national ID = %x / %x", got.TCKNEnc, got.TCKNBidx)
		}
		dup := party(parties.KindPayee, parties.LegalIndividual, nil, "")
		dup.TCKNEnc, dup.TCKNBidx = []byte("sealed-2"), bytes.Repeat([]byte{1}, 32)
		if _, err := s.InsertParty(t.Context(), dup); !errors.Is(err, parties.ErrTaxIDTaken) {
			t.Fatalf("duplicate blind index: %v", err)
		}
		// legal_parties_tckn_chk: an individual has both, nobody else has either.
		half := party(parties.KindPayee, parties.LegalIndividual, nil, "")
		half.TCKNEnc = []byte("sealed-3")
		if _, err := s.InsertParty(t.Context(), half); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("individual without blind index: %v", err)
		}
		co := party(parties.KindPayee, parties.LegalFoundation, nil, vknB)
		co.TCKNEnc, co.TCKNBidx = []byte("sealed-4"), bytes.Repeat([]byte{2}, 32)
		if _, err := s.InsertParty(t.Context(), co); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("national ID on a foundation: %v", err)
		}
	})

	t.Run("platform party", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		if _, err := s.GetPlatform(t.Context()); !errors.Is(err, parties.ErrNotFound) {
			t.Fatalf("GetPlatform before the seed: %v", err)
		}
		p := party(parties.KindPlatform, parties.LegalCompany, nil, "")
		p.Status, p.Address = parties.StatusDraft, parties.Address{}
		got, created, err := s.InsertPlatformIfAbsent(t.Context(), p)
		if err != nil || !created || got.ID != p.ID || got.Status != parties.StatusDraft {
			t.Fatalf("first insert: %+v %v %v", got, created, err)
		}
		again := p
		again.ID = uuid.NewV7()
		got, created, err = s.InsertPlatformIfAbsent(t.Context(), again)
		if err != nil || created || got.ID != p.ID {
			t.Fatalf("second insert: %+v %v %v", got, created, err)
		}
		if _, err := s.InsertParty(t.Context(), again); !errors.Is(err, parties.ErrPlatformExists) {
			t.Fatalf("second platform party: %v", err)
		}
		if got, err := s.GetPlatform(t.Context()); err != nil || got.ID != p.ID {
			t.Fatalf("GetPlatform = %+v %v", got, err)
		}
	})

	t.Run("org scope: own parties and payees", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		company, club, uni, by := w.org(t), w.org(t), w.org(t), w.user(t)
		buyerOld := insert(t, s, party(parties.KindBuyerCompany, parties.LegalCompany, &company, vknA))
		buyerNew := insert(t, s, party(parties.KindBuyerCompany, parties.LegalCompany, &company, vknB))
		found := insert(t, s, party(parties.KindPayee, parties.LegalFoundation, nil, vknC))
		uniOwn := insert(t, s, party(parties.KindPayee, parties.LegalUniversity, &uni, vknD))
		closed := insert(t, s, party(parties.KindPayee, parties.LegalAssociation, nil, vknE))
		_ = by
		w.payee(t, club, found.ID, "active")
		w.payee(t, uni, uniOwn.ID, "pending_verification")
		w.payee(t, club, closed.ID, "closed")

		scopes := func(org uuid.UUID) map[uuid.UUID]parties.Scope {
			t.Helper()
			ops, err := s.OrgParties(t.Context(), org)
			if err != nil {
				t.Fatal(err)
			}
			out := map[uuid.UUID]parties.Scope{}
			for _, op := range ops {
				out[op.Party.ID] = op.Scope
			}
			return out
		}
		if got := scopes(company); len(got) != 2 || got[buyerOld.ID] != parties.ScopeOrg || got[buyerNew.ID] != parties.ScopeOrg {
			t.Fatalf("company scope: %v", got)
		}
		if got := scopes(club); len(got) != 1 || got[found.ID] != parties.ScopePayee {
			t.Fatalf("club scope: %v", got)
		}
		if got := scopes(uni); len(got) != 1 || got[uniOwn.ID] != parties.ScopePayee {
			t.Fatalf("university scope (payee wins over org_id): %v", got)
		}
		for _, lock := range []bool{false, true} {
			op, err := s.OrgParty(t.Context(), club, found.ID, lock)
			if err != nil || op.Party.ID != found.ID || op.Scope != parties.ScopePayee {
				t.Fatalf("OrgParty(lock=%v) = %+v %v", lock, op, err)
			}
			if _, err := s.OrgParty(t.Context(), club, buyerOld.ID, lock); !errors.Is(err, parties.ErrNotFound) {
				t.Fatalf("another org's party: %v", err)
			}
			if _, err := s.OrgParty(t.Context(), club, closed.ID, lock); !errors.Is(err, parties.ErrNotFound) {
				t.Fatalf("party of a closed payee: %v", err)
			}
		}
		if _, err := s.OrgParties(t.Context(), uuid.Nil()); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("OrgParties without org: %v", err)
		}
		if _, err := s.OrgParty(t.Context(), uuid.Nil(), found.ID, false); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("OrgParty without org: %v", err)
		}
		bs, err := s.BuyerParties(t.Context(), company)
		if err != nil || len(bs) != 2 || bs[0].ID != buyerNew.ID || bs[1].ID != buyerOld.ID {
			t.Fatalf("BuyerParties = %+v %v", bs, err)
		}
		if _, err := s.BuyerParties(t.Context(), uuid.Nil()); !errors.Is(err, db.ErrMissingOrgScope) {
			t.Fatalf("BuyerParties without org: %v", err)
		}
		// Admin list: filters, title search (Turkish-insensitive), VKN, keyset.
		list := func(f parties.ListFilter, after *uuid.UUID, limit int) []uuid.UUID {
			t.Helper()
			ps, err := s.ListParties(t.Context(), f, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			out := make([]uuid.UUID, len(ps))
			for i, p := range ps {
				out[i] = p.ID
			}
			return out
		}
		all := []uuid.UUID{closed.ID, uniOwn.ID, found.ID, buyerNew.ID, buyerOld.ID}
		if got := list(parties.ListFilter{}, nil, 10); !slices.Equal(got, all) {
			t.Fatalf("ListParties = %v, want %v", got, all)
		}
		if got := list(parties.ListFilter{}, &all[1], 2); !slices.Equal(got, all[2:4]) {
			t.Fatalf("ListParties after = %v", got)
		}
		if got := list(parties.ListFilter{Kind: parties.KindBuyerCompany}, nil, 10); !slices.Equal(got, all[3:]) {
			t.Fatalf("kind filter = %v", got)
		}
		if got := list(parties.ListFilter{Q: vknC}, nil, 10); !slices.Equal(got, []uuid.UUID{found.ID}) {
			t.Fatalf("VKN search = %v", got)
		}
		if got := list(parties.ListFilter{Q: "ÖRNEK ünvan"}, nil, 10); len(got) != 5 {
			t.Fatalf("title search = %v", got)
		}
		if got := list(parties.ListFilter{Q: "yok_böyle%"}, nil, 10); len(got) != 0 {
			t.Fatalf("LIKE metacharacters must not match: %v", got)
		}
		if got := list(parties.ListFilter{Status: parties.StatusVerified}, nil, 10); len(got) != 0 {
			t.Fatalf("status filter = %v", got)
		}
	})

	t.Run("documents", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		by, verifier := w.user(t), w.user(t)
		p := insert(t, s, party(parties.KindPayee, parties.LegalFoundation, nil, vknA))
		q := insert(t, s, party(parties.KindPayee, parties.LegalFoundation, nil, vknB))
		f1, f2 := w.file(t), w.file(t)
		d1, err := s.InsertDocument(t.Context(), parties.Document{ID: uuid.NewV7(), LegalPartyID: p.ID, Kind: parties.DocFoundationDeed,
			FileID: f1, UploadedBy: by})
		if err != nil || d1.CreatedAt.IsZero() || d1.VerifiedAt != nil {
			t.Fatalf("InsertDocument = %+v %v", d1, err)
		}
		if _, err := s.InsertDocument(t.Context(), parties.Document{ID: uuid.NewV7(), LegalPartyID: q.ID, Kind: parties.DocOther,
			FileID: f1, UploadedBy: by}); !errors.Is(err, files.ErrAlreadyAttached) {
			t.Fatalf("one file, two documents: %v", err)
		}
		d2, err := s.InsertDocument(t.Context(), parties.Document{ID: uuid.NewV7(), LegalPartyID: q.ID, Kind: parties.DocTaxCertificate,
			FileID: f2, UploadedBy: by})
		if err != nil {
			t.Fatal(err)
		}
		for _, lock := range []bool{false, true} {
			if got, err := s.GetDocument(t.Context(), d1.ID, lock); err != nil || got.ID != d1.ID || got.FileID != f1 {
				t.Fatalf("GetDocument = %+v %v", got, err)
			}
		}
		if _, err := s.GetDocument(t.Context(), uuid.NewV7(), false); !errors.Is(err, parties.ErrNotFound) {
			t.Fatalf("missing document: %v", err)
		}
		ds, err := s.PartyDocuments(t.Context(), []uuid.UUID{p.ID, q.ID})
		if err != nil || len(ds) != 2 || ds[0].ID != d1.ID || ds[1].ID != d2.ID {
			t.Fatalf("PartyDocuments = %+v %v", ds, err)
		}
		if ds, err := s.PartyDocuments(t.Context(), nil); err != nil || len(ds) != 0 {
			t.Fatalf("PartyDocuments(nil) = %+v %v", ds, err)
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		v, err := s.SetDocumentVerified(t.Context(), d1.ID, verifier, at)
		if err != nil || v.VerifiedAt == nil || !v.VerifiedAt.Equal(at) || *v.VerifiedBy != verifier {
			t.Fatalf("SetDocumentVerified = %+v %v", v, err)
		}
		if _, err := s.SetDocumentVerified(t.Context(), uuid.NewV7(), verifier, at); !errors.Is(err, parties.ErrNotFound) {
			t.Fatalf("verify a missing document: %v", err)
		}
		mine, err := s.DocumentsUploadedBy(t.Context(), by)
		if err != nil || len(mine) != 2 {
			t.Fatalf("DocumentsUploadedBy = %+v %v", mine, err)
		}
	})
}
