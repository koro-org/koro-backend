package main

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/platform/civil"
	"koro/internal/students"
)

// matrixDiscoveryText is the body of fixture notice_discovery;
// bodies/students/update_privacy_discoverable.json holds its SHA-256.
const matrixDiscoveryText = "# Keşif profili rızası\n\nProfilimin takma adla şirketlerin keşif ekranında görünmesine izin veriyorum. (Matris fikstürü)\n"

// matrixFixtures seeds fixtures/students.yaml (03 §7.3): an experience of
// the persona student (with the profile it belongs to, at the persona's
// university) and the published discovery_profile consent text in the
// consent store installed by useMemStore.
func (w *studentsWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	for _, f := range fx.Of("students") {
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		switch f.Kind {
		case "experience":
			owner, _ := f.Fields["owner"].(string)
			if err := w.matrixExperience(ctx, f.Name, id, owner); err != nil {
				return err
			}
		case "consent_text":
			if err := w.matrixConsentText(ctx, f, id); err != nil {
				return err
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}

func (w *studentsWiring) matrixExperience(ctx context.Context, name string, id uuid.UUID, ownerName string) error {
	p, ok := authztest.Get(ownerName)
	if !ok || p.Actor.UniversityID == nil {
		return fmt.Errorf("fixture %s: the owner must be a verified student persona", name)
	}
	owner := p.User()
	if _, _, err := w.store.InsertProfile(ctx, students.Profile{UserID: owner, UniversityID: *p.Actor.UniversityID,
		DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive, CompletedSections: []string{"experience"}}); err != nil {
		return fmt.Errorf("fixture %s: profile: %w", name, err)
	}
	start, err := civil.New(2025, time.March, 1)
	if err != nil {
		return err
	}
	if _, err := w.store.InsertExperience(ctx, students.Experience{ID: id, UserID: owner, Kind: students.KindProject,
		Title: "Matris projesi", StartMonth: start}); err != nil {
		return fmt.Errorf("fixture %s: %w", name, err)
	}
	return nil
}

func (w *studentsWiring) matrixConsentText(ctx context.Context, f fixture, id uuid.UUID) error {
	if w.late == nil || w.late.consent.store == nil {
		return fmt.Errorf("fixture %s: no consent store", f.Name)
	}
	purpose, _ := f.Fields["purpose"].(string)
	code := consent.Purpose(purpose)
	base := time.Now().UTC().Truncate(time.Microsecond)
	n := consent.Notice{
		ID: id, Kind: consent.KindConsentText, PurposeCode: &code, Audience: consent.Audience("student"),
		Locale: consent.LocaleTR, Version: "2026.10.04", Title: "Matris " + f.Name, BodyMD: matrixDiscoveryText,
		BodySHA256: consent.BodyDigest(matrixDiscoveryText), CreatedAt: base.Add(-48 * time.Hour),
	}
	st := w.late.consent.store
	if err := st.InsertNotice(ctx, n); err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	if err := st.Publish(ctx, id, base.Add(-24*time.Hour)); err != nil {
		return fmt.Errorf("fixture %s: publish: %w", f.Name, err)
	}
	return nil
}
