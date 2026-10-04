package events

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/idempotency"
)

// lintRuleAggType is the audit resource type of a lint rule.
const lintRuleAggType = "form_lint_rule"

// lintAdmin is the authz resource of /admin/form-lint-rules.
var lintAdmin = authz.Resource{Type: authz.TypeAdminSurface}

// LintRuleInput is the body of adminCreateFormLintRule (04 §2.6).
type LintRuleInput struct {
	Class         LintClass
	Pattern       string
	DescriptionTR string
}

// LintRulePatch is the body of adminUpdateFormLintRule: nil fields stay.
// Rules are never deleted; Active=false retires one.
type LintRulePatch struct {
	Class         *LintClass
	Pattern       *string
	DescriptionTR *string
	Active        *bool
}

func (p LintRulePatch) empty() bool {
	return p.Class == nil && p.Pattern == nil && p.DescriptionTR == nil && p.Active == nil
}

// ListLintRules implements Service: every rule, active or not.
func (s *service) ListLintRules(ctx context.Context, a authz.Actor) ([]LintRule, error) {
	if err := authz.Can(ctx, a, authz.EventsLintRuleManage, lintAdmin); err != nil {
		return nil, err
	}
	return s.d.Store.ListLintRules(ctx)
}

func checkClass(c LintClass) error {
	if !c.Valid() {
		return fieldErr("body.class", "must be blocked, warn_special or review_identifier")
	}
	return nil
}

// CreateLintRule implements Service: the pattern must compile as RE2 and
// must not match the empty text (422).
func (s *service) CreateLintRule(ctx context.Context, a authz.Actor, in LintRuleInput) (LintRule, error) {
	if err := authz.Can(ctx, a, authz.EventsLintRuleManage, lintAdmin); err != nil {
		return LintRule{}, err
	}
	if err := checkClass(in.Class); err != nil {
		return LintRule{}, err
	}
	pattern, err := checkPattern("body.pattern", in.Pattern)
	if err != nil {
		return LintRule{}, err
	}
	desc, err := cleanText("body.description_tr", in.DescriptionTR, minLintDescription, maxLintDescription)
	if err != nil {
		return LintRule{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op string
		In LintRuleInput
	}{"create_lint_rule", in})
	if err != nil {
		return LintRule{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (LintRule, error) {
		r, err := s.d.Store.InsertLintRule(ctx, LintRule{
			ID: uuid.NewV7(), Class: in.Class, Pattern: pattern, DescriptionTR: desc, Active: true, UpdatedBy: &a.UserID,
		})
		if err != nil {
			return LintRule{}, err
		}
		return r, s.d.Audit.Log(ctx, lintAudit(a, r, map[string]any{"class": string(r.Class), "active": r.Active}))
	})
	return out, err
}

// UpdateLintRule implements Service: class, pattern, description and the
// active flag; a new pattern is checked like on creation.
func (s *service) UpdateLintRule(ctx context.Context, a authz.Actor, id uuid.UUID, p LintRulePatch) (LintRule, error) {
	if err := authz.Can(ctx, a, authz.EventsLintRuleManage, lintAdmin); err != nil {
		return LintRule{}, err
	}
	if p.empty() {
		return LintRule{}, ErrEmptyPatch
	}
	var out LintRule
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		r, err := s.d.Store.GetLintRule(ctx, id)
		if err != nil {
			return err
		}
		changed := []string{}
		if p.Class != nil {
			if err := checkClass(*p.Class); err != nil {
				return err
			}
			r.Class, changed = *p.Class, append(changed, "class")
		}
		if p.Pattern != nil {
			if r.Pattern, err = checkPattern("body.pattern", *p.Pattern); err != nil {
				return err
			}
			changed = append(changed, "pattern")
		}
		if p.DescriptionTR != nil {
			if r.DescriptionTR, err = cleanText("body.description_tr", *p.DescriptionTR, minLintDescription, maxLintDescription); err != nil {
				return err
			}
			changed = append(changed, "description_tr")
		}
		if p.Active != nil {
			r.Active, changed = *p.Active, append(changed, "active")
		}
		r.UpdatedBy = &a.UserID
		if out, err = s.d.Store.UpdateLintRule(ctx, r); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, lintAudit(a, out, map[string]any{
			"fields": changed, "class": string(out.Class), "active": out.Active,
		}))
	})
	if err != nil {
		return LintRule{}, err
	}
	return out, nil
}

// lintAudit is the audit record of a lint rule write (no org).
func lintAudit(a authz.Actor, r LintRule, meta map[string]any) audit.Event {
	id := r.ID
	return audit.Event{
		Action: string(authz.EventsLintRuleManage), Outcome: audit.Success, ResourceType: lintRuleAggType,
		ResourceID: &id, Metadata: meta,
	}.By(a)
}
