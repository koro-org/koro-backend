package identity_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/identity"
)

func wantUnrevoked(t *testing.T, e *env, user uuid.UUID, want ...uuid.UUID) {
	t.Helper()
	got, err := e.mod.UnrevokedSessionIDs(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("unrevoked sessions = %v, want %v", got, want)
	}
	for _, id := range want {
		if !slices.Contains(got, id) {
			t.Fatalf("unrevoked sessions = %v, want %v", got, want)
		}
	}
}

// TestUnrevokedSessionIDs_FollowsTheSessionsLife (BE-182): the sessions of the
// user alone; a sign-out, a revocation and a password change close theirs.
func TestUnrevokedSessionIDs_FollowsTheSessionsLife(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "live@example.com")
	other := e.activeUser(t, "other@example.com")
	wantUnrevoked(t, e, u.ID)

	web, webActor := e.login(t, "live@example.com", identity.ClientWeb)
	mob, _ := e.login(t, "live@example.com", identity.ClientMobile)
	third, _ := e.login(t, "live@example.com", identity.ClientWeb)
	theirs, _ := e.login(t, "other@example.com", identity.ClientWeb)
	wantUnrevoked(t, e, u.ID, web.Session.ID, mob.Session.ID, third.Session.ID)
	wantUnrevoked(t, e, other.ID, theirs.Session.ID)

	if err := e.svc.RevokeMySession(ctx, webActor, third.Session.ID); err != nil {
		t.Fatal(err)
	}
	wantUnrevoked(t, e, u.ID, web.Session.ID, mob.Session.ID)

	// A password change keeps the session that made it.
	if err := e.svc.ChangePassword(ctx, webActor, identity.PasswordChange{CurrentPassword: goodPassword, NewPassword: "Yeni-Ve-Uzun-Parola-4821"}); err != nil {
		t.Fatal(err)
	}
	wantUnrevoked(t, e, u.ID, web.Session.ID)

	if err := e.svc.DeleteCurrentSession(ctx, webActor); err != nil {
		t.Fatal(err)
	}
	wantUnrevoked(t, e, u.ID)
	wantUnrevoked(t, e, other.ID, theirs.Session.ID)
}

// TestUnrevokedSessionIDs_AdminActionsCloseThem: suspension and an admin
// revocation leave none.
func TestUnrevokedSessionIDs_AdminActionsCloseThem(t *testing.T) {
	t.Parallel()
	for name, act := range map[string]func(t *testing.T, s *staffEnv){
		"suspend": func(t *testing.T, s *staffEnv) {
			t.Helper()
			if _, err := s.svc.SuspendUser(t.Context(), s.a, s.target.ID, "spam reports"); err != nil {
				t.Fatal(err)
			}
		},
		"revoke": func(t *testing.T, s *staffEnv) {
			t.Helper()
			if _, err := s.svc.RevokeUserSessions(t.Context(), s.a, s.target.ID, "support request"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newStaffEnv(t)
			if ids, _ := s.mod.UnrevokedSessionIDs(t.Context(), s.target.ID); len(ids) == 0 {
				t.Fatal("the target has no session to close")
			}
			act(t, s)
			wantUnrevoked(t, s.env, s.target.ID)
		})
	}
}

// TestUnrevokedSessionIDs_ExpiredUntilTheRowGoes: an expired session counts
// until its row is removed. The session windows of the MFA roles are too short
// to end a device's notifications with them.
func TestUnrevokedSessionIDs_ExpiredUntilTheRowGoes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	u := e.activeUser(t, "idle@example.com")
	res, _ := e.login(t, "idle@example.com", identity.ClientWeb)
	e.clk.Advance(7*24*time.Hour + time.Second)
	wantUnrevoked(t, e, u.ID, res.Session.ID)
	if n, err := e.store.DeleteEndedSessions(t.Context(), e.clk.Now(), 10); err != nil || n != 1 {
		t.Fatalf("DeleteEndedSessions() = %d, %v", n, err)
	}
	wantUnrevoked(t, e, u.ID)
}
