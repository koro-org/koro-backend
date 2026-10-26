package directory

import (
	"context"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// SkillQuery narrows listSkills (04 §2.3: q, category) and adminListSkills
// (+ state).
type SkillQuery struct {
	Q        string
	Category SkillCategory
	// State is adminListSkills' choice (StateAll by default there); the
	// public listSkills always lists the active skills.
	State ActiveState
	Limit int
	After *After
}

// SkillPage is a page of skills.
type SkillPage struct {
	Items []Skill
	Next  *After
}

// SkillInput is the body of adminCreateSkill.
type SkillInput struct {
	Slug     string
	NameTR   string
	NameEN   string
	Category SkillCategory
}

// SkillPatch is the body of adminUpdateSkill: nil leaves a field as it is;
// an empty name_en clears it.
type SkillPatch struct {
	NameTR   *string
	NameEN   *string
	Category *SkillCategory
	IsActive *bool
}

func (p SkillPatch) empty() bool {
	return p.NameTR == nil && p.NameEN == nil && p.Category == nil && p.IsActive == nil
}

// ListSkills implements Service: active skills only.
func (s *service) ListSkills(ctx context.Context, a authz.Actor, q SkillQuery) (SkillPage, error) {
	if err := authz.Can(ctx, a, authz.DirectorySkillList, catalogResource); err != nil {
		return SkillPage{}, err
	}
	q.State = StateActive
	return s.listSkills(ctx, q)
}

// AdminListSkills implements Service (BE-128): the skills of q.State,
// both names, for the taxonomy's managers.
func (s *service) AdminListSkills(ctx context.Context, a authz.Actor, q SkillQuery) (SkillPage, error) {
	if err := authz.Can(ctx, a, authz.DirectorySkillManage, adminSurface); err != nil {
		return SkillPage{}, err
	}
	return s.listSkills(ctx, q)
}

// listSkills is one page of skills after the caller's authz.
func (s *service) listSkills(ctx context.Context, q SkillQuery) (SkillPage, error) {
	text, err := cleanQuery("query.q", q.Q)
	if err != nil {
		return SkillPage{}, err
	}
	if q.Category != "" && !q.Category.Valid() {
		return SkillPage{}, problem.Field("query.category", "unknown category")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListSkills(ctx, SkillFilter{Q: text, Category: q.Category, State: q.State}, q.After, limit+1)
	if err != nil {
		return SkillPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := SkillPage{Items: rows}
	if more {
		out.Next = &After{ID: rows[len(rows)-1].ID}
	}
	return out, nil
}

// cleanSkill validates the fields of a new skill.
func cleanSkill(in SkillInput, prefix string) (Skill, error) {
	if !skillSlugRe.MatchString(in.Slug) || len(in.Slug) > maxSlug {
		return Skill{}, problem.Field(prefix+"slug", "must be lower-case letters and digits separated by single hyphens, at most 80")
	}
	nameTR, err := cleanText(prefix+"name_tr", in.NameTR, 1, maxName)
	if err != nil {
		return Skill{}, err
	}
	nameEN, err := cleanOptional(prefix+"name_en", in.NameEN, maxName)
	if err != nil {
		return Skill{}, err
	}
	if !in.Category.Valid() {
		return Skill{}, problem.Field(prefix+"category", "must be one of technical, tool, soft, domain, language_tool, other")
	}
	return Skill{Slug: in.Slug, NameTR: nameTR, NameEN: nameEN, Category: in.Category, IsActive: true}, nil
}

// AdminCreateSkill implements Service.
func (s *service) AdminCreateSkill(ctx context.Context, a authz.Actor, in SkillInput) (Skill, error) {
	if err := authz.Can(ctx, a, authz.DirectorySkillManage, adminSurface); err != nil {
		return Skill{}, err
	}
	k, err := cleanSkill(in, "body.")
	if err != nil {
		return Skill{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op string
		K  Skill
	}{"create_skill", k})
	if err != nil {
		return Skill{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		k.ID = uuid.NewV7()
		created, err := s.d.Store.InsertSkill(ctx, k)
		if err != nil {
			return uuid.Nil(), err
		}
		return created.ID, s.d.Audit.Log(ctx, auditEvent(a, authz.DirectorySkillManage, "skill", created.ID, nil, "",
			map[string]any{"operation": "create_skill", "slug": created.Slug}))
	})
	if err != nil {
		return Skill{}, err
	}
	return s.d.Store.GetSkill(ctx, id)
}

// AdminUpdateSkill implements Service (deactivation included: skills are
// never deleted).
func (s *service) AdminUpdateSkill(ctx context.Context, a authz.Actor, id uuid.UUID, p SkillPatch) (Skill, error) {
	if err := authz.Can(ctx, a, authz.DirectorySkillManage, adminSurface); err != nil {
		return Skill{}, err
	}
	if p.empty() {
		return Skill{}, ErrEmptyPatch
	}
	var out Skill
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		k, err := s.d.Store.GetSkill(ctx, id)
		if err != nil {
			return err
		}
		var changed []string
		if p.NameTR != nil {
			if k.NameTR, err = cleanText("body.name_tr", *p.NameTR, 1, maxName); err != nil {
				return err
			}
			changed = append(changed, "name_tr")
		}
		if p.NameEN != nil {
			if k.NameEN, err = cleanOptional("body.name_en", *p.NameEN, maxName); err != nil {
				return err
			}
			changed = append(changed, "name_en")
		}
		if p.Category != nil {
			if !p.Category.Valid() {
				return problem.Field("body.category", "must be one of technical, tool, soft, domain, language_tool, other")
			}
			k.Category, changed = *p.Category, append(changed, "category")
		}
		if p.IsActive != nil {
			k.IsActive, changed = *p.IsActive, append(changed, "is_active")
		}
		if out, err = s.d.Store.UpdateSkill(ctx, k); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditEvent(a, authz.DirectorySkillManage, "skill", k.ID, nil, "",
			map[string]any{"operation": "update_skill", "fields": changed}))
	})
	return out, err
}
