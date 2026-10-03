package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/token"
)

// matrixOrgTime is created_at/updated_at of every matrix org: fixed, so
// the ETag of an untouched fixture org is a constant the matrix files send
// as If-Match ("1hmhuo3hji000" is base32 of its UnixNano; 04 §1.13).
var matrixOrgTime = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// matrixSlug is the slug of an org fixture: its name with hyphens.
func matrixSlug(name string) string { return strings.ReplaceAll(name, "_", "-") }

// matrixFixtures seeds the orgs of the matrix (03 §7.3): the shared org
// fixtures of authztest (active and verified; companies with a profile),
// the fixtures of fixtures/orgs.yaml (kind org: type, status; BE-019:
// persona_membership, membership, invitation) and one active org_members
// row per membership of every persona and helper user (company_owner_a
// owns company_a). The club facts come from directory's fixtures
// (hooks.clubInfo); until BE-018 it serves the personas' student
// verification (seam students of orgsWiring; never set in production).
func (w *orgsWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	insert := func(name string, kind orgs.Kind, status orgs.Status) error {
		id, err := fx.ID(name)
		if err != nil {
			return err
		}
		t := matrixOrgTime
		o := orgs.Org{ID: id, Kind: kind, Name: "Matris " + name, Slug: matrixSlug(name), Status: status,
			CreatedAt: t, UpdatedAt: t}
		if status != orgs.StatusPendingReview {
			o.VerifiedAt = &t
		}
		if _, ok, err := w.store.InsertOrg(ctx, o); err != nil || !ok {
			return fmt.Errorf("org %s: ok=%v err=%w", name, ok, err)
		}
		if kind == orgs.KindCompany {
			sector, size := "software", "11-50"
			if err := w.store.InsertCompany(ctx, orgs.Company{OrgID: id, SectorCode: &sector, SizeBand: &size}); err != nil {
				return fmt.Errorf("company %s: %w", name, err)
			}
		}
		return nil
	}
	for _, o := range authztest.Orgs() {
		if err := insert(o.Name, o.Kind, orgs.StatusActive); err != nil {
			return err
		}
	}
	named := map[string]uuid.UUID{} // "<persona>/<org id>" → membership fixture ID
	var later []fixture
	for _, f := range fx.Of("orgs") {
		switch f.Kind {
		case "org":
			typ, _ := f.Fields["type"].(string)
			status, _ := f.Fields["status"].(string)
			if err := insert(f.Name, orgs.Kind(typ), orgs.Status(status)); err != nil {
				return err
			}
		case "persona_membership":
			persona, _ := f.Fields["persona"].(string)
			org, err := fx.Ref(f, "org")
			if err != nil {
				return err
			}
			id, err := fx.ID(f.Name)
			if err != nil {
				return err
			}
			named[persona+"/"+org.String()] = id
		case "membership", "invitation":
			later = append(later, f)
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		for _, m := range p.Actor.Memberships {
			t := matrixOrgTime
			id, ok := named[p.Name+"/"+m.OrgID.String()]
			if !ok {
				id = authztest.ID(p.Name + "_membership_" + m.OrgID.String())
			}
			if _, err := w.store.InsertMember(ctx, orgs.Membership{
				ID: id, OrgID: m.OrgID, UserID: p.User(),
				Role: m.Role, Status: orgs.MemberActive, Source: matrixSource(m.Role), JoinedAt: &t,
			}); err != nil {
				return fmt.Errorf("membership of %s: %w", p.Name, err)
			}
		}
	}
	for _, f := range later {
		if err := w.matrixRow(ctx, fx, f); err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
	}
	if w.students == nil {
		w.students = &matrixStudents{lapsed: map[uuid.UUID]bool{}}
	}
	return nil
}

// matrixRow seeds a membership or an invitation fixture.
func (w *orgsWiring) matrixRow(ctx context.Context, fx *matrixFixtures, f fixture) error {
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	org, err := fx.Ref(f, "org")
	if err != nil {
		return err
	}
	role, _ := f.Fields["role"].(string)
	t := matrixOrgTime
	if f.Kind == "membership" {
		user, err := fx.Ref(f, "user")
		if err != nil {
			return err
		}
		status, _ := f.Fields["status"].(string)
		m := orgs.Membership{ID: id, OrgID: org, UserID: user, Role: authz.MemberRole(role), Status: orgs.MemberStatus(status),
			Source: orgs.SourceJoinRequest, RequestedAt: &t}
		if m.Status == orgs.MemberActive {
			m.JoinedAt = &t
		}
		_, err = w.store.InsertMember(ctx, m)
		return err
	}
	by, err := fx.Ref(f, "invited_by")
	if err != nil {
		return err
	}
	email, _ := f.Fields["email"].(string)
	expires := matrixOrgTime.Add(30 * 24 * time.Hour) // the test clock stands at 2026-10-03
	if past, _ := f.Fields["expires"].(string); past == "past" {
		expires = matrixOrgTime
	}
	_, err = w.store.InsertInvitation(ctx, orgs.Invitation{
		ID: id, OrgID: org, Email: email, EmailNormalized: email, Role: authz.MemberRole(role), KeyID: "test1",
		TokenHash: token.Hash(matrixToken(f.Name)), InvitedBy: by, ExpiresAt: expires,
	})
	return err
}

func matrixSource(r authz.MemberRole) orgs.Source {
	if r == authz.MemberClubAdmin {
		return orgs.SourceClaim
	}
	return orgs.SourceAdmin
}

// matrixStudents is the personas' student verification (03 §7.3: every
// persona with a university is a verified student); a scenario hook makes
// one lapse.
type matrixStudents struct {
	mu     sync.Mutex
	lapsed map[uuid.UUID]bool
}

func (s *matrixStudents) Active(_ context.Context, userID uuid.UUID) (identity.EduVerification, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lapsed[userID] {
		return identity.EduVerification{}, false, nil
	}
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.User() == userID && p.Actor.UniversityID != nil {
			return identity.EduVerification{UniversityID: *p.Actor.UniversityID, ExpiresAt: time.Now().Add(365 * 24 * time.Hour)}, true, nil
		}
	}
	return identity.EduVerification{}, false, nil
}

func (s *matrixStudents) ActiveMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.EduVerification, error) {
	out := map[uuid.UUID]identity.EduVerification{}
	for _, id := range ids {
		if v, ok, _ := s.Active(ctx, id); ok {
			out[id] = v
		}
	}
	return out, nil
}

// matrixHook runs the orgs scenario hooks:
//
//	edu_lapsed {persona}  the persona's student verification lapses and
//	                      user.edu_verification_lapsed reaches the orgs worker
//	                      (03 §7.7 "Rol düşmesi").
func (w *orgsWiring) matrixHook(fx *matrixFixtures, name string, args map[string]any) error {
	switch name {
	case "edu_lapsed":
		persona, _ := args["persona"].(string)
		user, err := fx.ID(persona)
		if err != nil {
			return err
		}
		st, ok := w.students.(*matrixStudents)
		if !ok {
			return errors.New("edu_lapsed: the student verification is not the matrix's")
		}
		st.mu.Lock()
		st.lapsed[user] = true
		st.mu.Unlock()
		env, err := domainevent.New(domainevent.UserEduVerificationLapsed{UserID: user, VerificationID: uuid.NewV7()}, "user", user)
		if err != nil {
			return err
		}
		env.ID, env.OccurredAt = uuid.NewV7(), time.Now()
		b, err := json.Marshal(env)
		if err != nil {
			return err
		}
		r := outbox.NewRegistry()
		w.mod.Subscribe(r)
		return r.For(domainevent.TopicUserEduVerificationLapsed)[0].HandleTask(context.Background(), b)
	}
	return fmt.Errorf("unknown orgs hook %q", name)
}
