package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/identity"
	"koro/internal/platform/civil"
	"koro/internal/students"
)

// The directory service is the students SkillNamer (BE-084): checked at
// compile time, so the binding in wire_students.go never silently drops.
var _ students.SkillNamer = directory.Service(nil)

// skillNameCounter counts the directory store's skill name queries.
type skillNameCounter struct {
	directory.Store
	calls *atomic.Int32
}

func (s skillNameCounter) SkillNamesTR(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	s.calls.Add(1)
	return s.Store.SkillNamesTR(ctx, ids)
}

// TestWire_DirectoryNamesStudentSkills (BE-084) through the built
// container: students' company views name skills through directory's
// Catalog.SkillNames — a level 2 search page of 10 profiles costs one
// directory query (no N+1) and so does a level 3 profile; an inactive
// catalog skill keeps an empty name.
func TestWire_DirectoryNamesStudentSkills(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	calls := &atomic.Int32{}
	mem := c.directory.newStore
	c.directory.newStore = func(c *container) directory.Store {
		return skillNameCounter{Store: mem(c), calls: calls}
	}
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	now := c.infra.clock.Now().UTC().Truncate(time.Microsecond)

	var skills []directory.Skill
	for i, name := range []string{"Go", "SQL", "Eski Beceri"} {
		k, err := c.directory.built.InsertSkill(ctx, directory.Skill{ID: uuid.NewV7(), Slug: fmt.Sprintf("be084-skill-%d", i),
			NameTR: name, Category: "technical", IsActive: i < 2})
		if err != nil {
			t.Fatal(err)
		}
		skills = append(skills, k)
	}

	// The published discovery_profile text the consents point at.
	purpose := consent.PurposeDiscoveryProfile
	body := "Seviye 2 keşif izni (BE-084 test metni)."
	notice := consent.Notice{ID: uuid.NewV7(), Kind: consent.KindConsentText, PurposeCode: &purpose, Audience: consent.AudienceStudent,
		Locale: consent.LocaleTR, Version: "2026.10.23", Title: "Keşif", BodyMD: body, BodySHA256: consent.BodyDigest(body)}
	if err := c.consent.store.InsertNotice(ctx, notice); err != nil {
		t.Fatal(err)
	}
	if err := c.consent.store.Publish(ctx, notice.ID, now); err != nil {
		t.Fatal(err)
	}

	uni := uuid.NewV7()
	adult, _ := civil.New(2000, time.March, 14)
	exp := now.AddDate(1, 0, 0)
	var users []uuid.UUID
	for i := range 10 {
		id := uuid.NewV7()
		email := "be084-" + id.String() + "@example.test"
		if err := c.identity.store.InsertUser(ctx, identity.User{ID: id, Status: identity.StatusActive, Email: email,
			EmailNormalized: email, EmailVerifiedAt: &now, FirstName: "Öğrenci", LastName: "Test", Persona: identity.PersonaStudent,
			Locale: identity.LocaleTR, BirthDate: &adult}); err != nil {
			t.Fatal(err)
		}
		addr := "be084-" + id.String() + "@matrix.edu.tr"
		if err := c.identity.store.InsertEduVerification(ctx, identity.EduRecord{ID: uuid.NewV7(), UserID: id,
			Method: identity.EduMethodEmail, EduEmail: addr, EduEmailNormalized: addr, EmailDomain: "matrix.edu.tr",
			UniversityID: &uni, Status: identity.EduVerified, VerifiedAt: &now, ExpiresAt: &exp}); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := c.students.store.InsertProfile(ctx, students.Profile{UserID: id, UniversityID: uni,
			DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}); err != nil || !ok {
			t.Fatalf("InsertProfile: ok=%v err=%v", ok, err)
		}
		pr, err := c.students.store.LockPrivacy(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		pr.Discoverable = true
		if _, err := c.students.store.UpdatePrivacy(ctx, pr); err != nil {
			t.Fatal(err)
		}
		if err := c.consent.store.InsertConsent(ctx, consent.Consent{ID: uuid.NewV7(), UserID: id, PurposeCode: purpose,
			Action: consent.Granted, NoticeVersionID: &notice.ID, TextSHA256: notice.BodySHA256, ScopeType: consent.ScopeNone,
			Channel: consent.ChannelWeb, OccurredAt: now}); err != nil {
			t.Fatal(err)
		}
		held := []students.Skill{{UserID: id, SkillID: skills[i%2].ID}, {UserID: id, SkillID: skills[2].ID}}
		if _, err := c.students.store.ReplaceSkills(ctx, id, held); err != nil {
			t.Fatal(err)
		}
		users = append(users, id)
	}
	want := map[string]string{skills[0].ID.String(): "Go", skills[1].ID.String(): "SQL", skills[2].ID.String(): ""}

	calls.Store(0)
	page, err := c.students.svc.Search(ctx, students.DiscoveryQuery{Limit: 10})
	if err != nil || len(page.Items) != 10 || page.BelowThreshold {
		t.Fatalf("level 2 search = %d items, below=%v, %v", len(page.Items), page.BelowThreshold, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("directory skill name queries for a page of 10 = %d, want 1", n)
	}
	for _, it := range page.Items {
		d, err := it.WithAnonID("x")
		if err != nil || len(d.Skills) != 2 {
			t.Fatalf("level 2 profile = %+v, %v", d, err)
		}
		for _, k := range d.Skills {
			if w, ok := want[k.SkillID]; !ok || k.Name != w {
				t.Fatalf("level 2 skill %s named %q, want %q", k.SkillID, k.Name, w)
			}
		}
	}

	company := uuid.NewV7()
	if err := c.students.svc.Grant(ctx, users[0], company, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	rec := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), StudentStatus: authz.StudentNone,
		Memberships: []authz.Membership{{OrgID: company, OrgKind: authz.OrgCompany, Role: authz.MemberRecruiter}}}
	calls.Store(0)
	l3, err := c.students.svc.GetCompanyStudentProfile(ctx, rec, company, users[0])
	if err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("directory skill name queries for a level 3 profile = %d, want 1", n)
	}
	if len(l3.Skills) != 2 {
		t.Fatalf("level 3 skills = %+v", l3.Skills)
	}
	for _, k := range l3.Skills {
		if w, ok := want[k.SkillID]; !ok || k.Name != w {
			t.Fatalf("level 3 skill %s named %q, want %q", k.SkillID, k.Name, w)
		}
	}
}
