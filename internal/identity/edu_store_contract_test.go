package identity_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/identity"
)

// eduWorld is one fresh store with the rows edu_verifications refers to:
// a university (FK) and edu_document files (FK) of a user.
type eduWorld struct {
	s    identity.Store
	uni  func(t *testing.T) uuid.UUID
	file func(t *testing.T, owner uuid.UUID) uuid.UUID
}

func eduEmailRow(userID, uni uuid.UUID, address string, status identity.EduStatus, at time.Time) identity.EduRecord {
	v := identity.EduRecord{ID: uuid.NewV7(), UserID: userID, Method: identity.EduMethodEmail, EduEmail: address,
		EduEmailNormalized: address, EmailDomain: "uni.edu.tr", UniversityID: &uni, Status: status, CreatedAt: at, UpdatedAt: at}
	if status == identity.EduVerified || status == identity.EduExpired || status == identity.EduSuperseded {
		exp := at.AddDate(1, 0, 0)
		v.VerifiedAt, v.ExpiresAt = &at, &exp
	}
	return v
}

// eduStoreContract is the edu_verifications contract both stores pass
// (05 §13.3): the in-memory one here, the GORM one in
// edu_store_integration_test.go.
func eduStoreContract(t *testing.T, newWorld func(t *testing.T) eduWorld) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		s, ctx := w.s, t.Context()
		u, v, reviewer := contractUser(uuid.NewV7().String()+"@c.test"), contractUser(uuid.NewV7().String()+"@c.test"),
			contractUser(uuid.NewV7().String()+"@c.test")
		for _, x := range []identity.User{u, v, reviewer} {
			if err := s.InsertUser(ctx, x); err != nil {
				t.Fatal(err)
			}
		}
		uni := w.uni(t)
		addr := uuid.NewV7().String() + "@uni.edu.tr"
		first := eduEmailRow(u.ID, uni, addr, identity.EduVerified, contractBase)
		if err := s.InsertEduVerification(ctx, first); err != nil {
			t.Fatal(err)
		}
		got, err := s.EduVerification(ctx, first.ID)
		if err != nil || got.EduEmail != addr || *got.UniversityID != uni || !got.ExpiresAt.Equal(*first.ExpiresAt) || got.Status != identity.EduVerified {
			t.Fatalf("EduVerification = %+v, %v", got, err)
		}
		// One .edu.tr address verifies one account; one verified row per user.
		if err := s.InsertEduVerification(ctx, eduEmailRow(v.ID, uni, addr, identity.EduVerified, contractBase)); !errors.Is(err, identity.ErrEduEmailTaken) {
			t.Fatalf("taken address = %v, want ErrEduEmailTaken", err)
		}
		if h, found, _ := s.OpenEduVerificationOfEmail(ctx, addr); !found || h.UserID != u.ID {
			t.Fatalf("holder of the address = %+v %v", h, found)
		}
		// A pending document of u (one pending per user).
		file := w.file(t, u.ID)
		doc := identity.EduRecord{ID: uuid.NewV7(), UserID: u.ID, Method: identity.EduMethodDocument, UniversityID: &uni,
			DocumentFileID: &file, Status: identity.EduPending, CreatedAt: contractBase, UpdatedAt: contractBase}
		if err := s.InsertEduVerification(ctx, doc); err != nil {
			t.Fatal(err)
		}
		file2 := w.file(t, u.ID)
		dup := doc
		dup.ID, dup.DocumentFileID = uuid.NewV7(), &file2
		if err := s.InsertEduVerification(ctx, dup); !errors.Is(err, identity.ErrEduPending) {
			t.Fatalf("second pending = %v, want ErrEduPending", err)
		}
		if x, found, _ := s.EduVerificationOfFile(ctx, file); !found || x.ID != doc.ID {
			t.Fatalf("verification of the file = %+v %v", x, found)
		}
		open, err := s.LockOpenEduVerifications(ctx, u.ID)
		if err != nil || len(open) != 2 {
			t.Fatalf("open rows = %d, %v", len(open), err)
		}
		// A pending document never loses its file; a decided one may.
		noFile := doc
		noFile.DocumentFileID = nil
		if err := s.UpdateEduVerification(ctx, noFile); err == nil {
			t.Fatal("a pending document lost its file")
		}
		// Nobody reviews their own document (03 Y6).
		self := doc
		self.Status, self.ReviewedBy = identity.EduRejected, &u.ID
		if err := s.UpdateEduVerification(ctx, self); !errors.Is(err, identity.ErrForbidden) {
			t.Fatalf("self review = %v, want ErrForbidden", err)
		}
		// Approval: the old row superseded first, then the document verified.
		at := contractBase.Add(time.Hour)
		first.Status, first.UpdatedAt = identity.EduSuperseded, at
		if err := s.UpdateEduVerification(ctx, first); err != nil {
			t.Fatal(err)
		}
		exp := at.AddDate(1, 0, 0)
		doc.Status, doc.VerifiedAt, doc.ExpiresAt, doc.ReviewedBy, doc.ReviewNote, doc.UpdatedAt = identity.EduVerified, &at, &exp, &reviewer.ID, "tamam", at
		if err := s.UpdateEduVerification(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if out, found, _ := s.LatestEduOutcome(ctx, u.ID); !found || out.ID != doc.ID {
			t.Fatalf("latest outcome = %+v %v", out, found)
		}
		if _, found, _ := s.OpenEduVerificationOfEmail(ctx, addr); found {
			t.Fatal("a superseded row still holds the address")
		}
		active, err := s.ActiveEduVerifications(ctx, []uuid.UUID{u.ID, v.ID}, at)
		if err != nil || len(active) != 1 || active[0].ID != doc.ID {
			t.Fatalf("active = %+v, %v", active, err)
		}
		if a, _ := s.ActiveEduVerifications(ctx, []uuid.UUID{u.ID}, exp); len(a) != 0 {
			t.Fatal("active at its expiry")
		}
		// The retention may drop a decided document's file.
		doc.DocumentFileID = nil
		if err := s.UpdateEduVerification(ctx, doc); err != nil {
			t.Fatalf("decided document without its file: %v", err)
		}
		all, _ := s.EduVerificationsOf(ctx, u.ID)
		if len(all) != 2 || all[0].ID != doc.ID || all[0].ReviewNote != "tamam" || all[0].DocumentFileID != nil {
			t.Fatalf("EduVerificationsOf = %+v", all)
		}
		// Reminders and expiry.
		if r, _ := s.EduExpiringBetween(ctx, exp.Add(-time.Hour), exp.Add(time.Hour), nil, 10); len(r) != 1 || r[0].ID != doc.ID {
			t.Fatalf("expiring = %+v", r)
		}
		if r, _ := s.ExpireEduVerifications(ctx, exp.Add(-time.Second), 10); len(r) != 0 {
			t.Fatalf("expired early: %+v", r)
		}
		r, err := s.ExpireEduVerifications(ctx, exp, 10)
		if err != nil || len(r) != 1 || r[0].ID != doc.ID || r[0].Status != identity.EduExpired {
			t.Fatalf("expire = %+v, %v", r, err)
		}
		if out, found, _ := s.LatestEduOutcome(ctx, u.ID); !found || out.Status != identity.EduExpired {
			t.Fatalf("latest outcome after the expiry = %+v", out)
		}
		if r, _ := s.ExpireEduVerifications(ctx, exp.Add(time.Hour), 10); len(r) != 0 {
			t.Fatal("expired twice")
		}
		// Erasure deletes every row and returns them.
		del, err := s.DeleteUserEduVerifications(ctx, u.ID)
		if err != nil || len(del) != 2 {
			t.Fatalf("deleted %d, %v", len(del), err)
		}
		if _, err := s.EduVerification(ctx, doc.ID); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("after erasure = %v", err)
		}
	})
	t.Run("queue", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		s, ctx := w.s, t.Context()
		uni := w.uni(t)
		var pending []uuid.UUID
		for range 3 {
			x := contractUser(uuid.NewV7().String() + "@c.test")
			if err := s.InsertUser(ctx, x); err != nil {
				t.Fatal(err)
			}
			f := w.file(t, x.ID)
			d := identity.EduRecord{ID: uuid.NewV7(), UserID: x.ID, Method: identity.EduMethodDocument, UniversityID: &uni,
				DocumentFileID: &f, Status: identity.EduPending, CreatedAt: contractBase, UpdatedAt: contractBase}
			if err := s.InsertEduVerification(ctx, d); err != nil {
				t.Fatal(err)
			}
			pending = append(pending, d.ID)
		}
		// Oldest first, keyset after the previous page.
		p1, err := s.EduQueue(ctx, identity.EduQuery{Status: identity.EduPending, Limit: 2})
		if err != nil || len(p1) != 2 || p1[0].ID != pending[0] || p1[1].ID != pending[1] {
			t.Fatalf("page 1 = %v, %v", p1, err)
		}
		p2, _ := s.EduQueue(ctx, identity.EduQuery{Status: identity.EduPending, After: &p1[1].ID, Limit: 2})
		if len(p2) != 1 || p2[0].ID != pending[2] {
			t.Fatalf("page 2 = %v", p2)
		}
		if r, _ := s.EduQueue(ctx, identity.EduQuery{Status: identity.EduRejected, Limit: 5}); len(r) != 0 {
			t.Fatalf("rejected = %v", r)
		}
		if ok, _ := s.DeleteEduVerification(ctx, pending[0]); !ok {
			t.Fatal("DeleteEduVerification missed")
		}
		rest, _ := s.EduQueue(ctx, identity.EduQuery{Status: identity.EduPending, Limit: 5})
		if slices.ContainsFunc(rest, func(r identity.EduRecord) bool { return r.ID == pending[0] }) {
			t.Fatal("deleted row still queued")
		}
	})
}

func TestEduStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	eduStoreContract(t, func(*testing.T) eduWorld {
		return eduWorld{
			s:    identity.NewMemStore(),
			uni:  func(*testing.T) uuid.UUID { return uuid.NewV7() },
			file: func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() },
		}
	})
}
