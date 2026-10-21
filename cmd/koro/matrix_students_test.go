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
// university), the published discovery_profile consent text in the
// consent store installed by useMemStore, the student's current CV and the
// level 3 grants of the companies (BE-046; a suspended one, BE-077).
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
		case "cv":
			if err := w.matrixCV(ctx, fx, f, id); err != nil {
				return err
			}
		case "grant":
			if err := w.matrixGrant(ctx, fx, f, id); err != nil {
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

// matrixCV inserts the owner's profile (when missing) and a current CV
// holding the files fixture named by "file" (BE-025).
func (w *studentsWiring) matrixCV(ctx context.Context, fx *matrixFixtures, f fixture, id uuid.UUID) error {
	ownerName, _ := f.Fields["owner"].(string)
	p, ok := authztest.Get(ownerName)
	if !ok || p.Actor.UniversityID == nil {
		return fmt.Errorf("fixture %s: the owner must be a verified student persona", f.Name)
	}
	file, err := fx.Ref(f, "file")
	if err != nil {
		return err
	}
	owner := p.User()
	if _, _, err := w.store.InsertProfile(ctx, students.Profile{UserID: owner, UniversityID: *p.Actor.UniversityID,
		DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive, CompletedSections: []string{"cv"}}); err != nil {
		return fmt.Errorf("fixture %s: profile: %w", f.Name, err)
	}
	if _, err := w.store.InsertCV(ctx, students.CV{ID: id, UserID: owner, FileID: file}); err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	if _, err := w.store.SetCurrentCV(ctx, owner, id); err != nil {
		return fmt.Errorf("fixture %s: current: %w", f.Name, err)
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

// matrixGrant inserts the student's profile (when missing) and an active
// level 3 grant of the company, as an accepted contact request leaves it
// (BE-046; the request ID is authztest.ID(<name>_request)). suspended: true
// is a grant whose student's verification lapsed (BE-077): the owner is an
// account without a verification (its profile at uni_a, from when it was
// verified), the grant given an hour ago and suspended now.
func (w *studentsWiring) matrixGrant(ctx context.Context, fx *matrixFixtures, f fixture, id uuid.UUID) error {
	studentName, _ := f.Fields["student"].(string)
	suspended, _ := f.Fields["suspended"].(bool)
	p, ok := authztest.Get(studentName)
	switch {
	case !ok || !p.Actor.IsUser() || p.Actor.IsMinor:
		return fmt.Errorf("fixture %s: the student must be an adult account", f.Name)
	case suspended == p.Actor.IsVerifiedStudent():
		return fmt.Errorf("fixture %s: an active grant needs a verified student, a suspended one a lapsed account", f.Name)
	}
	company, err := fx.Ref(f, "company")
	if err != nil {
		return err
	}
	student, uni := p.User(), authztest.UniversityID("uni_a")
	if p.Actor.UniversityID != nil {
		uni = *p.Actor.UniversityID
	}
	if _, _, err := w.store.InsertProfile(ctx, students.Profile{UserID: student, UniversityID: uni,
		DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}); err != nil {
		return fmt.Errorf("fixture %s: profile: %w", f.Name, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	granted := now
	if suspended {
		granted = now.Add(-time.Hour)
	}
	if _, ok, err := w.store.InsertGrant(ctx, students.AccessGrant{ID: id, StudentUserID: student, CompanyOrgID: company,
		Source: students.SourceContactRequest, SourceID: authztest.ID(f.Name + "_request"),
		GrantedAt: granted}); err != nil || !ok {
		return fmt.Errorf("fixture %s: grant: ok=%v %w", f.Name, ok, err)
	}
	if suspended {
		if got, err := w.store.SuspendGrants(ctx, student, now); err != nil || len(got) != 1 {
			return fmt.Errorf("fixture %s: suspend: %d rows, %w", f.Name, len(got), err)
		}
	}
	return nil
}
