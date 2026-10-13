package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/feed"
	"koro/internal/outreach"
)

// matrixSegmentTime dates every segment fixture: the ETag of an untouched
// segment is "1hmhuo3hji000" (outreach.yaml's If-Match).
var matrixSegmentTime = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// matrixFixtures seeds fixtures/outreach.yaml into the in-memory store
// installed by useMemStore: segments with a below-threshold estimate; BE-035
// targeted posts (into the feed's in-memory store) and dispatches.
func (w *outreachWiring) matrixFixtures(fx *matrixFixtures) error {
	mem, ok := w.store.(*outreach.MemStore)
	if !ok {
		return errors.New("outreach fixtures need the in-memory store (useMemStore)")
	}
	for _, f := range fx.Of("outreach") {
		switch f.Kind {
		case "segment":
		case "targeted_post":
			if err := w.matrixTargetedPost(fx, f); err != nil {
				return err
			}
			continue
		case "dispatch":
			if err := matrixDispatch(fx, f, mem); err != nil {
				return err
			}
			continue
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		org, err := fx.Ref(f, "org")
		if err != nil {
			return err
		}
		by, err := fx.Ref(f, "created_by")
		if err != nil {
			return err
		}
		var def outreach.Definition
		if def.Uni, err = matrixIDs(fx, f, "uni"); err != nil {
			return err
		}
		if def.Dept, err = matrixIDs(fx, f, "dept"); err != nil {
			return err
		}
		if def.Club, err = matrixIDs(fx, f, "club"); err != nil {
			return err
		}
		years, _ := f.Fields["year"].([]any)
		for _, y := range years {
			n, ok := y.(int)
			if !ok {
				return fmt.Errorf("fixture %s: year %v is not a number", f.Name, y)
			}
			def.Year = append(def.Year, int16(n))
		}
		at := matrixSegmentTime
		seg := outreach.Segment{
			ID: id, OrgID: org, Name: "Matris " + f.Name, Definition: def, AudienceKeys: def.Keys(),
			AudienceDimCount: def.DimCount(), LastCountedAt: &at, CreatedBy: by, CreatedAt: at, UpdatedAt: at,
		}
		if archived, _ := f.Fields["archived"].(bool); archived {
			seg.ArchivedAt = &at
		}
		if err := mem.Put(seg); err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
	}
	return nil
}

// matrixDefinition reads the uni, dept, club and year fields of f.
func matrixDefinition(fx *matrixFixtures, f fixture) (outreach.Definition, error) {
	var (
		def outreach.Definition
		err error
	)
	if def.Uni, err = matrixIDs(fx, f, "uni"); err != nil {
		return def, err
	}
	if def.Dept, err = matrixIDs(fx, f, "dept"); err != nil {
		return def, err
	}
	if def.Club, err = matrixIDs(fx, f, "club"); err != nil {
		return def, err
	}
	years, _ := f.Fields["year"].([]any)
	for _, y := range years {
		n, ok := y.(int)
		if !ok {
			return def, fmt.Errorf("fixture %s: year %v is not a number", f.Name, y)
		}
		def.Year = append(def.Year, int16(n))
	}
	return def, nil
}

// matrixTargetedPost puts a published targeted post into the feed's
// in-memory store (BE-035): the keys of its definition, a company's post
// commercial with an estimate and a usage (posts_company_segment_chk).
func (w *outreachWiring) matrixTargetedPost(fx *matrixFixtures, f fixture) error {
	if w.late == nil {
		return errors.New("outreach fixtures: the container is not built")
	}
	fs, ok := w.late.feed.built.(*feed.MemStore)
	if !ok {
		return fmt.Errorf("fixture %s: the feed's in-memory store is needed, got %T", f.Name, w.late.feed.built)
	}
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	org, err := fx.Ref(f, "org")
	if err != nil {
		return err
	}
	author, err := fx.Ref(f, "author")
	if err != nil {
		return err
	}
	def, err := matrixDefinition(fx, f)
	if err != nil {
		return err
	}
	snap, err := json.Marshal(def)
	if err != nil {
		return err
	}
	kind, _ := f.Fields["author_kind"].(string)
	t, title, text := matrixSegmentTime, "Matris "+f.Name, "Matris hedefli gönderisi"
	p := feed.Post{
		ID: id, OrgID: org, AuthorOrgKind: feed.AuthorKind(kind), AuthorUserID: author, Kind: feed.KindText, Title: &title,
		BodyJSON: feed.JSON(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Matris"}]}]}`),
		BodyText: &text, AudienceKind: feed.AudienceSegment, AudienceKeys: def.Keys(), AudienceDimCount: def.DimCount(),
		AudienceSnapshot: snap, CommercialFlag: kind == string(feed.AuthorCompany), Status: feed.StatusPublished,
		PublishedAt: &t, CreatedAt: t, UpdatedAt: t,
	}
	if kind == string(feed.AuthorCompany) {
		est, usage := 40, authztest.ID(f.Name+"_usage")
		p.AudienceSizeEstimate, p.EntitlementUsageID = &est, &usage
	}
	if err := fs.PutPost(p); err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	return nil
}

// matrixDispatch puts a dispatch of a targeted post fixture (BE-035).
func matrixDispatch(fx *matrixFixtures, f fixture, mem *outreach.MemStore) error {
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	org, err := fx.Ref(f, "org")
	if err != nil {
		return err
	}
	post, err := fx.Ref(f, "post")
	if err != nil {
		return err
	}
	by, err := fx.Ref(f, "created_by")
	if err != nil {
		return err
	}
	status, _ := f.Fields["status"].(string)
	t := matrixSegmentTime
	d := outreach.Dispatch{
		ID: id, OrgID: org, PostID: post, AudienceKeys: []string{"uni:" + authztest.UniversityID("uni_a").String()},
		AudienceDimCount: 1, Channels: []string{"push"}, TitleSnapshot: "Matris", DeepLink: "/posts/" + post.String(),
		Commercial: true, Status: outreach.DispatchStatus(status), CreatedBy: by, CreatedAt: t, UpdatedAt: t,
	}
	if d.Status.Terminal() {
		d.CompletedAt = &t
	}
	for field, dst := range map[string]**int{"eligible": &d.EligibleCount, "consented": &d.ConsentedCount, "sent": &d.SentCount} {
		if n, ok := f.Fields[field].(int); ok {
			v := n
			*dst = &v
		}
	}
	if err := mem.PutDispatch(d); err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	return nil
}

// matrixIDs resolves a fixture field listing fixture names.
func matrixIDs(fx *matrixFixtures, f fixture, field string) ([]uuid.UUID, error) {
	names, _ := f.Fields[field].([]any)
	var out []uuid.UUID
	for _, n := range names {
		name, ok := n.(string)
		if !ok {
			return nil, fmt.Errorf("fixture %s: %s must list fixture names", f.Name, field)
		}
		id, err := fx.ID(name)
		if err != nil {
			return nil, fmt.Errorf("fixture %s: %w", f.Name, err)
		}
		out = append(out, id)
	}
	return out, nil
}
