package main

import (
	"errors"
	"fmt"
	"time"
	"uuid"

	"koro/internal/outreach"
)

// matrixSegmentTime dates every segment fixture: the ETag of an untouched
// segment is "1hmhuo3hji000" (outreach.yaml's If-Match).
var matrixSegmentTime = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// matrixFixtures seeds fixtures/outreach.yaml into the in-memory store
// installed by useMemStore: segments with a below-threshold estimate.
func (w *outreachWiring) matrixFixtures(fx *matrixFixtures) error {
	mem, ok := w.store.(*outreach.MemStore)
	if !ok {
		return errors.New("outreach fixtures need the in-memory store (useMemStore)")
	}
	for _, f := range fx.Of("outreach") {
		if f.Kind != "segment" {
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
