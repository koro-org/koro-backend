package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/talent"
)

// matrixTalentAt is the instant of every talent fixture (before every
// matrix request).
var matrixTalentAt = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

// matrixTalentParams are the params of talent_config_v1: test values of
// the matrix (the product values come with createTalentConfigVersion).
const matrixTalentParams = `{"shrink_k":5,"cohort_min":30,"lookback_days":540,
"category_caps_cp":{"participation":4000,"intent":1500,"profile":1000,"community":600,"engagement":300},
"bands":[{"band":1,"min_pct_bp":5000,"mult_bp":10500},{"band":2,"min_pct_bp":7500,"mult_bp":11000},{"band":3,"min_pct_bp":9000,"mult_bp":11500}]}`

// matrixConfig applies talent_auditor_api_enabled (03 §5.12 k-flag; its
// default false is in _personas.yaml) before build.
func (w *talentWiring) matrixConfig(cfg *config.Modules, key string, v any) (bool, error) {
	if key != "talent_auditor_api_enabled" {
		return false, nil
	}
	on, ok := v.(bool)
	if !ok {
		return true, fmt.Errorf("talent_auditor_api_enabled must be a boolean, got %T", v)
	}
	cfg.Talent.AuditorAPIEnabled = on
	return true, nil
}

// matrixFixtures seeds fixtures/talent.yaml (03 §7.3) into the in-memory
// store installed by useMemStore, in a talent transaction (the store
// refuses anything else): the config version first, then what refers to it.
func (w *talentWiring) matrixFixtures(fx *matrixFixtures) error {
	st, ok := w.store.(*talent.MemStore)
	if !ok {
		return errors.New("talent store is not in memory")
	}
	rank := map[string]int{"config_version": 0, "point_event": 1, "score": 2, "neutralization": 3}
	fixtures := fx.Of("talent")
	slices.SortStableFunc(fixtures, func(a, b fixture) int { return cmp.Compare(rank[a.Kind], rank[b.Kind]) })
	return db.NopTx{}.WithTx(context.Background(), func(ctx context.Context) error {
		ctx, err := st.Enter(ctx)
		if err != nil {
			return err
		}
		for _, f := range fixtures {
			if err := matrixTalentFixture(ctx, st, fx, f); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		}
		return nil
	})
}

func matrixTalentFixture(ctx context.Context, st *talent.MemStore, fx *matrixFixtures, f fixture) error {
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	switch f.Kind {
	case "config_version":
		by, err := fx.Ref(f, "by")
		if err != nil {
			return err
		}
		_, err = st.InsertConfigVersion(ctx, talent.ConfigVersion{ID: id, EffectiveFrom: matrixTalentAt.Add(-24 * time.Hour),
			Params: json.RawMessage(matrixTalentParams), Notes: "Matris fikstürü", CreatedBy: by})
		return err
	case "point_event":
		user, err := fx.Ref(f, "user")
		if err != nil {
			return err
		}
		cfg, err := fx.Ref(f, "config")
		if err != nil {
			return err
		}
		signal, _ := f.Fields["signal"].(string)
		points, _ := f.Fields["points"].(int)
		_, _, err = st.InsertPoint(ctx, talent.PointEvent{ID: id, UserID: user, Signal: signal, SourceType: talent.SourceCheckin,
			SourceID: uuid.NewV7(), OccurredAt: matrixTalentAt, RawPointsCP: points, PointsCP: points, ReasonCode: signal,
			ConfigVersionID: cfg, SourceEventID: uuid.NewV7()})
		return err
	case "score":
		user, err := fx.Ref(f, "user")
		if err != nil {
			return err
		}
		band, _ := f.Fields["band"].(int)
		st.PutScore(user, talent.Band(band), false)
		return nil
	case "neutralization":
		user, err := fx.Ref(f, "user")
		if err != nil {
			return err
		}
		reason, _ := f.Fields["reason"].(string)
		n := talent.Neutralization{ID: id, UserID: user, Reason: talent.NeutralizeReason(reason)}
		if _, ok := f.Fields["by"]; ok {
			by, err := fx.Ref(f, "by")
			if err != nil {
				return err
			}
			n.CreatedBy = &by
		}
		if n.Reason == talent.NeutralizeObjection {
			dsr := uuid.NewV7()
			n.DSRID = &dsr
		}
		_, _, err = st.InsertNeutralization(ctx, n)
		return err
	}
	return fmt.Errorf("unknown kind %q", f.Kind)
}
