package directory_test

import (
	"context"
	"testing"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/identity"
)

// TestReauthDenied_EveryROperation (03 §6.2, BE-074): directory's R
// operation (directory.club.set_sensitivity) audits a refused proof —
// wrong (403 identity.reauth_failed) or missing (403
// identity.reauth_required) — as one denied reauth_failed record of the
// club. The proof is checked before the club is read, so the record has no
// org scope (06 H-38: never from the request) and no flag or reason:
// nothing happened. It writes nothing when identity cannot answer within
// the request budget (the bare deadline, httpx 503) and goes through with
// the right proof afterwards. Before BE-074 the refusal went back without
// a record.
func TestReauthDenied_EveryROperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	id := e.club(t, uni, "Düşünce Kulübü", directory.CategoryCulture).Club.OrgID
	a := pad(t)
	set := directory.SensitivityInput{IsSensitive: true, SensitiveReason: "philosophical_belief"}
	audittest.ReauthCase[identity.ReauthProof]{
		Call: func(ctx context.Context, p identity.ReauthProof) error {
			_, err := e.svc.AdminSetClubSensitivity(authz.WithActor(ctx, a), a, id, set, p)
			return err
		},
		Rows:   func() []audittest.Row { return e.audit.reauthRows(authz.DirectoryClubSetSensitivity) },
		Denied: rowOf(audit.Event{Action: string(authz.DirectoryClubSetSensitivity), ResourceType: authz.TypeClub, ResourceID: &id}.By(a)),
		Late:   e.reauth,
		Wrong:  identity.ReauthProof{Password: "yanlis-parola-1"}, WrongErr: identity.ErrReauthFailed,
		Missing: identity.ReauthProof{}, MissingErr: identity.ErrReauthRequired,
		Good: identity.ReauthProof{Password: goodPassword},
	}.Run(t)
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
