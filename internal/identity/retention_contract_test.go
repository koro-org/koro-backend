package identity_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
)

// retentionContract checks the retention part of Store (BE-057) on both
// stores: which rows each identity policy is due for, oldest first, and
// what a dry run counts.
func retentionContract(t *testing.T, newWorld func(t *testing.T) eduWorld) {
	t.Helper()
	newUser := func(t *testing.T, s identity.Store) uuid.UUID {
		t.Helper()
		u := contractUser(uuid.NewV7().String() + "@retention.test")
		if err := s.InsertUser(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}

	t.Run("student verifications and documents", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		s, ctx := w.s, t.Context()
		uni := w.uni(t)
		base := contractBase
		row := func(status identity.EduStatus, at time.Time) identity.EduRecord {
			t.Helper()
			v := eduEmailRow(newUser(t, s), uni, uuid.NewV7().String()+"@uni.edu.tr", status, at)
			if err := s.InsertEduVerification(ctx, v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		expiredLong := row(identity.EduExpired, base.AddDate(-3, 0, 0))    // expired base−2y: due
		superseded := row(identity.EduSuperseded, base.AddDate(0, -18, 0)) // ended base−18m: due
		row(identity.EduVerified, base.AddDate(0, -2, 0))                  // in force
		row(identity.EduExpired, base.AddDate(0, -13, 0))                  // expired a month ago

		// A document verification decided 2 months ago still holds its file.
		owner, reviewer := newUser(t, s), newUser(t, s)
		file := w.file(t, owner)
		verified, exp := base.AddDate(0, -2, 0), base.AddDate(0, 10, 0)
		doc := identity.EduRecord{ID: uuid.NewV7(), UserID: owner, Method: identity.EduMethodDocument, UniversityID: &uni,
			DocumentFileID: &file, Status: identity.EduVerified, VerifiedAt: &verified, ExpiresAt: &exp, ReviewedBy: &reviewer,
			CreatedAt: verified, UpdatedAt: verified}
		if err := s.InsertEduVerification(ctx, doc); err != nil {
			t.Fatal(err)
		}

		histCutoff := base.AddDate(-1, 0, 0)
		if n, err := s.CountEduRetentionDue(ctx, identity.PolicyEduVerificationHistory, histCutoff); err != nil || n != 2 {
			t.Fatalf("history due = %d, %v; want 2", n, err)
		}
		due, err := s.EduRetentionDue(ctx, identity.PolicyEduVerificationHistory, histCutoff, 1)
		if err != nil || len(due) != 1 || due[0].ID != expiredLong.ID {
			t.Fatalf("history batch of 1 = %+v, %v; want the longest expired", due, err)
		}
		if due, _ := s.EduRetentionDue(ctx, identity.PolicyEduVerificationHistory, histCutoff, 10); len(due) != 2 || due[1].ID != superseded.ID {
			t.Fatalf("history due = %+v", due)
		}
		docCutoff := base.AddDate(0, 0, -30)
		if n, err := s.CountEduRetentionDue(ctx, identity.PolicyEduDocument, docCutoff); err != nil || n != 1 {
			t.Fatalf("documents due = %d, %v; want 1", n, err)
		}
		due, err = s.EduRetentionDue(ctx, identity.PolicyEduDocument, docCutoff, 10)
		if err != nil || len(due) != 1 || due[0].ID != doc.ID || due[0].DocumentFileID == nil || *due[0].DocumentFileID != file {
			t.Fatalf("documents due = %+v, %v", due, err)
		}
		if n, _ := s.CountEduRetentionDue(ctx, identity.PolicyEduDocument, verified); n != 0 {
			t.Fatalf("a document decided at the cut-off is due: %d", n)
		}
		if _, err := s.CountEduRetentionDue(ctx, "no_such_policy", base); err == nil {
			t.Fatal("unknown policy accepted")
		}
	})

	t.Run("role history and privileged account requests", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		s, ctx := w.s, t.Context()
		requester, rejecter := newUser(t, s), newUser(t, s)
		old := contractBase.AddDate(-5, 0, 0)
		var rejected []uuid.UUID
		for i, at := range []time.Time{old, old.AddDate(0, 1, 0), contractBase} {
			ra := identity.RoleAssignment{ID: uuid.NewV7(), UserID: newUser(t, s), Role: authz.PlatformModerator, Reason: "moderation team",
				Status: identity.RolePending, RequestedBy: &requester, RequestedAt: at}
			if err := s.InsertRoleAssignment(ctx, ra); err != nil {
				t.Fatal(err)
			}
			ra.Status, ra.RejectedBy, ra.RejectedAt = identity.RoleRejected, &rejecter, &at
			if err := s.UpdateRoleAssignment(ctx, ra); err != nil {
				t.Fatalf("reject %d: %v", i, err)
			}
			rejected = append(rejected, ra.ID)
		}
		cutoff := contractBase.AddDate(-4, 0, 0)
		if n, err := s.PurgeExpired(ctx, identity.PolicyRoleHistory, cutoff, 0, true); err != nil || n != 2 {
			t.Fatalf("role history dry run = %d, %v; want 2", n, err)
		}
		if n, err := s.PurgeExpired(ctx, identity.PolicyRoleHistory, cutoff, 1, false); err != nil || n != 1 {
			t.Fatalf("role history batch = %d, %v", n, err)
		}
		if _, err := s.RoleAssignment(ctx, rejected[0]); err == nil {
			t.Fatal("the oldest rejected assignment survived the first batch")
		}
		if n, _ := s.PurgeExpired(ctx, identity.PolicyRoleHistory, cutoff, 10, false); n != 1 {
			t.Fatalf("second batch = %d, want 1", n)
		}
		if _, err := s.RoleAssignment(ctx, rejected[2]); err != nil {
			t.Fatalf("the young rejected assignment was purged: %v", err)
		}

		target := newUser(t, s)
		r := identity.AccountActionRequest{ID: uuid.NewV7(), TargetUserID: target, Action: identity.AccountSuspend, Reason: "suspicious",
			Status: identity.AccountActionPending, RequestedBy: requester, RequestedAt: old, ExpiresAt: old.Add(24 * time.Hour)}
		if err := s.InsertAccountAction(ctx, r); err != nil {
			t.Fatal(err)
		}
		// Lapsed 5 years ago: the dry run counts it (the real purge needs the
		// approval, which 000121's function checks).
		if n, err := s.PurgeExpired(ctx, identity.PolicyPrivilegedActionHistory, cutoff, 0, true); err != nil || n != 1 {
			t.Fatalf("privileged requests dry run = %d, %v; want 1", n, err)
		}
		if n, _ := s.PurgeExpired(ctx, identity.PolicyPrivilegedActionHistory, old, 0, true); n != 0 {
			t.Fatalf("a request that lapsed after the cut-off was counted: %d", n)
		}
		if _, err := s.PurgeExpired(ctx, "no_such_policy", cutoff, 1, true); err == nil {
			t.Fatal("unknown policy accepted")
		}
	})
}

func TestRetentionStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	retentionContract(t, func(*testing.T) eduWorld {
		return eduWorld{
			s:    identity.NewMemStore(),
			uni:  func(*testing.T) uuid.UUID { return uuid.NewV7() },
			file: func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() },
		}
	})
}
