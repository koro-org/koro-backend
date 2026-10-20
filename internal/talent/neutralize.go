package talent

import (
	"context"
	"fmt"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
	"koro/internal/students"
)

// neutralizeJob is the system actor of a neutralisation that comes
// through the kvkk hook.
const neutralizeJob = "talent.neutralize"

// Audit changes of a neutralisation (metadata "change").
const (
	changeNeutralized = "neutralized"
	changeLifted      = "lifted"
)

// handleRankingChoice is talent.apply_ranking_choice
// (student.ranking_choice_changed): the student's current choice is read
// again and applied, so two quick changes delivered out of order still end
// in the latest one. disabled → an opt-out neutralisation; enabled or
// default → the opt-out is lifted. Applies with the flag off too (it
// restricts the processing).
func (s *service) handleRankingChoice(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.StudentRankingChoiceChanged](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if err := system(ctx, ApplyRankingChoiceTaskName); err != nil {
		return queue.Permanent(err)
	}
	choice, err := s.d.Students.RankingChoice(ctx, p.UserID)
	if err != nil {
		return fmt.Errorf("talent: ranking choice: %w", err)
	}
	if choice == students.RankingDisabled {
		return s.neutralize(ctx, ApplyRankingChoiceTaskName, p.UserID, NeutralizeOptOut, nil)
	}
	return s.inTx(ctx, func(ctx context.Context) error {
		ns, err := s.d.Store.Neutralizations(ctx, p.UserID)
		if err != nil {
			return err
		}
		for _, n := range ns {
			if n.Reason != NeutralizeOptOut || !n.Active() {
				continue
			}
			row, lifted, err := s.d.Store.LiftNeutralization(ctx, n.ID, nil, s.now())
			if err != nil {
				return err
			}
			if lifted {
				if err := s.auditNeutralization(ctx, authz.SystemActor(ApplyRankingChoiceTaskName), row, changeLifted); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// handleObjection is talent.neutralize_objection (dsr.profiling_objection):
// the KVKK objection neutralises the subject for good (md. 11/1-g).
func (s *service) handleObjection(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.DSRProfilingObjection](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.DSRID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("talent: objection of %s without its request", p.UserID))
	}
	dsr := p.DSRID
	return s.neutralize(ctx, NeutralizeObjectionTaskName, p.UserID, NeutralizeObjection, &dsr)
}

// Neutralize implements Auditor.
func (s *service) Neutralize(ctx context.Context, userID uuid.UUID, reason NeutralizeReason, dsrID *uuid.UUID) error {
	return s.neutralize(ctx, neutralizeJob, userID, reason, dsrID)
}

// neutralize adds a neutralisation as system job job; an active one of the
// same reason is kept (idempotent) and nothing is audited then.
func (s *service) neutralize(ctx context.Context, job string, userID uuid.UUID, reason NeutralizeReason, dsrID *uuid.UUID) error {
	if !reason.Valid() || (reason == NeutralizeObjection && (dsrID == nil || *dsrID == uuid.Nil())) || userID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("%w: %q", ErrInvalidReason, reason))
	}
	if err := system(ctx, job); err != nil {
		return queue.Permanent(err)
	}
	return s.inTx(ctx, func(ctx context.Context) error {
		row, inserted, err := s.d.Store.InsertNeutralization(ctx, Neutralization{ID: uuid.NewV7(), UserID: userID, Reason: reason, DSRID: dsrID})
		if err != nil || !inserted {
			return err
		}
		return s.auditNeutralization(ctx, authz.SystemActor(job), row, changeNeutralized)
	})
}

// Lift implements Auditor. The authorisation comes first (a caller without
// the action learns nothing about the neutralisation), then the rule.
func (s *service) Lift(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	if err := authz.Can(ctx, a, authz.TalentNeutralizationManage, authz.Resource{Type: authz.TypeTalentSubject}); err != nil {
		return err
	}
	return s.inTx(ctx, func(ctx context.Context) error {
		n, err := s.d.Store.GetNeutralization(ctx, id)
		if err != nil {
			return err
		}
		if err := n.Liftable(); err != nil {
			return err
		}
		by := a.UserID
		row, lifted, err := s.d.Store.LiftNeutralization(ctx, id, &by, s.now())
		switch {
		case err != nil:
			return err
		case !lifted:
			return ErrNotActive
		}
		return s.auditNeutralization(ctx, a, row, changeLifted)
	})
}

// auditNeutralization writes the talent.neutralization.manage record in
// the talent transaction of ctx, on the session role.
func (s *service) auditNeutralization(ctx context.Context, a authz.Actor, n Neutralization, change string) error {
	meta := map[string]any{"change": change, "reason": string(n.Reason), "neutralization_id": n.ID.String()}
	if n.DSRID != nil {
		meta["dsr_id"] = n.DSRID.String()
	}
	uid := n.UserID
	return s.d.Store.AsApp(ctx, func(ctx context.Context) error {
		return s.d.Audit.Log(ctx, audit.Event{Action: string(authz.TalentNeutralizationManage), Outcome: audit.Success,
			ResourceType: authz.TypeTalentSubject, ResourceID: &uid, Metadata: meta}.By(a))
	})
}
