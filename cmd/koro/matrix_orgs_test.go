package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/orgs"
)

// matrixOrgTime is created_at/updated_at of every matrix org: fixed, so
// the ETag of an untouched fixture org is a constant the matrix files send
// as If-Match ("1hmhuo3hji000" is base32 of its UnixNano; 04 §1.13).
var matrixOrgTime = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// matrixSlug is the slug of an org fixture: its name with hyphens.
func matrixSlug(name string) string { return strings.ReplaceAll(name, "_", "-") }

// matrixFixtures seeds the orgs of the matrix (03 §7.3): the shared org
// fixtures of authztest (active and verified; companies with a profile),
// the extra orgs of fixtures/orgs.yaml (kind org: type, status) and one
// active org_members row per membership of every persona and helper user
// (company_owner_a owns company_a).
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
	for _, f := range fx.Of("orgs") {
		if f.Kind != "org" {
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
		typ, _ := f.Fields["type"].(string)
		status, _ := f.Fields["status"].(string)
		if err := insert(f.Name, orgs.Kind(typ), orgs.Status(status)); err != nil {
			return err
		}
	}
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		for _, m := range p.Actor.Memberships {
			t := matrixOrgTime
			if _, err := w.store.InsertMember(ctx, orgs.Membership{
				ID: authztest.ID(p.Name + "_membership_" + m.OrgID.String()), OrgID: m.OrgID, UserID: p.User(),
				Role: m.Role, Status: orgs.MemberActive, Source: matrixSource(m.Role), JoinedAt: &t,
			}); err != nil {
				return fmt.Errorf("membership of %s: %w", p.Name, err)
			}
		}
	}
	return nil
}

func matrixSource(r authz.MemberRole) orgs.Source {
	if r == authz.MemberClubAdmin {
		return orgs.SourceClaim
	}
	return orgs.SourceAdmin
}
