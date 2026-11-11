package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"koro/internal/authz/authztest"
	"koro/internal/ingest"
)

// matrixIngestStart is the start of the fixtures' imported events (after
// the matrix clock: they are upcoming).
var matrixIngestStart = time.Date(2026, 12, 1, 15, 0, 0, 0, time.UTC)

// matrixFixtures seeds fixtures/ingest.yaml (BE-210) into the in-memory
// store installed by useMemStore.
func (w *ingestWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	uni := authztest.UniversityID("uni_a")
	for _, f := range fx.Of("ingest") {
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		switch f.Kind {
		case "imported_event":
			club, err := fx.Ref(f, "club")
			if err != nil {
				return err
			}
			status, _ := f.Fields["status"].(string)
			sum := sha256.Sum256([]byte(f.Name))
			e := ingest.ImportedEvent{
				ID: id, UniversityID: uni, ClubOrgID: &club, Status: ingest.Status(status), Title: "Matris etkinliği " + f.Name,
				StartsAt: matrixIngestStart, Category: "talk", SourceURL: "https://kim.example.org/etkinlikler",
				Evidence: "Matris etkinliği 1 Aralık", Confidence: 0.9, Fingerprint: sum[:],
				ExpiresAt: matrixIngestStart.Add(8 * 24 * time.Hour), LastSeenAt: matrixIngestStart.Add(-30 * 24 * time.Hour),
			}
			if e.Status == ingest.StatusHidden {
				r := ingest.HiddenByRequest
				e.HiddenReason = &r
			}
			if err := w.store.InsertEvent(ctx, e); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "source":
			enabled, _ := f.Fields["enabled"].(bool)
			s := ingest.Source{
				ID: id, Kind: ingest.KindEvents, Adapter: ingest.AdapterHTMLPage, URL: "https://" + f.Name + ".example.org/e",
				Host: f.Name + ".example.org", Label: "Matris " + f.Name, UniversityID: uni, IntervalSeconds: 6 * 3600,
				Enabled: enabled,
			}
			if !enabled {
				r := ingest.PausedAdmin
				s.PausedReason = &r
			}
			if _, _, err := w.store.InsertSource(ctx, s); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "imported_club":
			club, err := fx.Ref(f, "club")
			if err != nil {
				return err
			}
			if err := w.store.UpsertImportedClub(ctx, ingest.ImportedClub{ClubOrgID: club, UniversityID: uni,
				NameKey: "matris " + f.Name, NeedsReview: true, FirstSeenAt: matrixIngestStart, LastSeenAt: matrixIngestStart}); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}
