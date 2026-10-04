package outreach

import (
	"context"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// ListSegments implements Service.
func (s *service) ListSegments(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ListQuery) (SegmentPage, error) {
	if err := authz.Can(ctx, a, authz.OutreachSegmentList, surface(orgID)); err != nil {
		return SegmentPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.List(ctx, orgID, q.Archived, q.After, limit+1)
	if err != nil {
		return SegmentPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := SegmentPage{Items: make([]SegmentView, len(rows))}
	for i, r := range rows {
		out.Items[i] = view(r)
	}
	if more {
		last := rows[len(rows)-1].ID
		out.Next = &last
	}
	return out, nil
}

// GetSegment implements Service.
func (s *service) GetSegment(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (SegmentView, error) {
	if err := authz.Can(ctx, a, authz.OutreachSegmentRead, surface(orgID)); err != nil {
		return SegmentView{}, err
	}
	seg, err := s.d.Store.Get(ctx, orgID, id)
	if err != nil {
		return SegmentView{}, err
	}
	return view(seg), nil
}

// CreateSegment implements Service. The definition is checked against the
// actor's scope (03 k16) and counted (one unit of the daily budget); a
// segment below k = 10 is saved with a NULL last_count.
func (s *service) CreateSegment(ctx context.Context, a authz.Actor, orgID uuid.UUID, in SegmentInput) (SegmentView, error) {
	if err := authz.Can(ctx, a, authz.OutreachSegmentManage, surface(orgID)); err != nil {
		return SegmentView{}, err
	}
	if err := s.usableOrg(ctx, a, orgID); err != nil {
		return SegmentView{}, err
	}
	name, err := cleanName("body.name", in.Name)
	if err != nil {
		return SegmentView{}, err
	}
	def, err := in.Definition.normalize("body.definition")
	if err != nil {
		return SegmentView{}, err
	}
	if err := s.checkScope(ctx, ScopeOf(a, orgID), orgID, def); err != nil {
		return SegmentView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op   string
		Org  uuid.UUID
		Name string
		Def  Definition
	}{"create", orgID, name, def})
	if err != nil {
		return SegmentView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		est, err := s.count(ctx, orgID, def)
		if err != nil {
			return uuid.Nil(), err
		}
		now := s.now()
		seg, err := s.d.Store.Insert(ctx, Segment{
			ID: uuid.NewV7(), OrgID: orgID, Name: name, Definition: def, AudienceKeys: def.Keys(),
			AudienceDimCount: def.DimCount(), LastCount: est.Count, LastCountedAt: &now, CreatedBy: a.UserID,
		})
		if err != nil {
			return uuid.Nil(), err
		}
		return seg.ID, s.d.Audit.Log(ctx, auditSegment(a, auditSegmentCreate, seg, shapeOf(def)))
	})
	if err != nil {
		return SegmentView{}, err
	}
	seg, err := s.d.Store.Get(ctx, orgID, id)
	if err != nil {
		return SegmentView{}, err
	}
	return view(seg), nil
}

// UpdateSegment implements Service. A definition equal to the current one
// is not counted again; a patch that changes nothing writes nothing (the
// ETag stays).
func (s *service) UpdateSegment(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, ifMatch string, p SegmentPatch) (SegmentView, error) {
	if err := authz.Can(ctx, a, authz.OutreachSegmentManage, surface(orgID)); err != nil {
		return SegmentView{}, err
	}
	if err := s.usableOrg(ctx, a, orgID); err != nil {
		return SegmentView{}, err
	}
	if p.Name == nil && p.Definition == nil {
		return SegmentView{}, ErrEmptyPatch
	}
	var (
		name string
		def  *Definition
	)
	if p.Name != nil {
		n, err := cleanName("body.name", *p.Name)
		if err != nil {
			return SegmentView{}, err
		}
		name = n
	}
	if p.Definition != nil {
		d, err := p.Definition.normalize("body.definition")
		if err != nil {
			return SegmentView{}, err
		}
		if err := s.checkScope(ctx, ScopeOf(a, orgID), orgID, d); err != nil {
			return SegmentView{}, err
		}
		def = &d
	}
	var out Segment
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.Lock(ctx, orgID, id)
		if err != nil {
			return err
		}
		if err := checkIfMatch(ifMatch, etagOf(cur.UpdatedAt)); err != nil {
			return err
		}
		if cur.Archived() {
			return ErrInvalidState
		}
		next, changed := cur, map[string]any{}
		if p.Name != nil && name != cur.Name {
			next.Name, changed["name"] = name, true
		}
		if def != nil && !sameDefinition(*def, cur.Definition) {
			est, err := s.count(ctx, orgID, *def)
			if err != nil {
				return err
			}
			now := s.now()
			next.Definition, next.AudienceKeys, next.AudienceDimCount = *def, def.Keys(), def.DimCount()
			next.LastCount, next.LastCountedAt = est.Count, &now
			changed["definition"] = true
			for k, v := range shapeOf(*def) {
				changed[k] = v
			}
		}
		if len(changed) == 0 {
			out = cur
			return nil
		}
		if out, err = s.d.Store.Update(ctx, next); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditSegment(a, auditSegmentUpdate, out, changed))
	})
	if err != nil {
		return SegmentView{}, err
	}
	return view(out), nil
}

// ArchiveSegment implements Service.
func (s *service) ArchiveSegment(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (SegmentView, error) {
	if err := authz.Can(ctx, a, authz.OutreachSegmentManage, surface(orgID)); err != nil {
		return SegmentView{}, err
	}
	if err := s.usableOrg(ctx, a, orgID); err != nil {
		return SegmentView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op  string
		Org uuid.UUID
		ID  uuid.UUID
	}{"archive", orgID, id})
	if err != nil {
		return SegmentView{}, err
	}
	if _, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.Lock(ctx, orgID, id)
		if err != nil || cur.Archived() {
			return cur.ID, err
		}
		now := s.now()
		next := cur
		next.ArchivedAt = &now
		out, err := s.d.Store.Update(ctx, next)
		if err != nil {
			return uuid.Nil(), err
		}
		return out.ID, s.d.Audit.Log(ctx, auditSegment(a, auditSegmentArchive, out, nil))
	}); err != nil {
		return SegmentView{}, err
	}
	seg, err := s.d.Store.Get(ctx, orgID, id)
	if err != nil {
		return SegmentView{}, err
	}
	return view(seg), nil
}

// shapeOf is the audited shape of a definition: which dimensions and how
// many values, never the values' audience or a count.
func shapeOf(d Definition) map[string]any {
	return map[string]any{
		"dimensions": int(d.DimCount()), "uni": len(d.Uni), "dept": len(d.Dept), "year": len(d.Year), "club": len(d.Club),
	}
}

// sameDefinition reports whether two normalized definitions are equal.
func sameDefinition(a, b Definition) bool {
	return slices.Equal(a.Uni, b.Uni) && slices.Equal(a.Dept, b.Dept) && slices.Equal(a.Year, b.Year) &&
		slices.Equal(a.Club, b.Club)
}
