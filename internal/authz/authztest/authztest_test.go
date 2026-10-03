package authztest_test

import (
	"crypto/sha256"
	"errors"
	"slices"
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

func TestID_DeterministicUUIDv5(t *testing.T) {
	t.Parallel()
	a, b := authztest.ID("club_a"), authztest.ID("club_a")
	if a != b || a == authztest.ID("club_b") {
		t.Fatal("ID must be deterministic and name-specific")
	}
	if a[6]>>4 != 5 || a[8]>>6 != 2 {
		t.Fatalf("ID(club_a) = %s is not an RFC 9562 version 5 UUID", a)
	}
	// RFC 9562 Appendix A.4 test vector: DNS namespace, "www.example.com".
	dns := [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	if got := authztest.NameUUID(dns, "www.example.com").String(); got != "2ed6657d-e927-568b-95e1-2665a8aea6a2" {
		t.Fatalf("NameUUID(DNS, www.example.com) = %s", got)
	}
}

func TestPersonas_Match03(t *testing.T) {
	t.Parallel()
	ps := authztest.Personas()
	if len(ps) != 18 {
		t.Fatalf("%d personas, want 18 (03 §1.1 + 2 IDOR personas)", len(ps))
	}
	ids := map[string]bool{}
	for i, p := range append(ps, authztest.Helpers()...) {
		if i < 18 && (p.Name != authztest.PersonaNames[i] || p.Helper) {
			t.Fatalf("persona %d = %s (helper=%v), want %s", i, p.Name, p.Helper, authztest.PersonaNames[i])
		}
		if p.Name == "anonymous" {
			if p.Token != "" || p.Actor.Kind != authz.KindAnonymous {
				t.Fatalf("anonymous persona = %+v", p)
			}
			continue
		}
		if p.Actor.Kind != authz.KindUser || p.Token == "" || ids[p.User().String()] {
			t.Fatalf("persona %s: kind %v token %q id %s", p.Name, p.Actor.Kind, p.Token, p.User())
		}
		ids[p.User().String()] = true
		// Sessions of MFA-required roles are MFA verified, others are not (03 §7.3).
		wantMFA := len(p.Actor.PlatformRoles) > 0 || slices.ContainsFunc(p.Actor.Memberships, func(m authz.Membership) bool {
			return m.Role == authz.MemberUniversityRep || m.Role == authz.MemberCompanyAdmin || m.Role == authz.MemberCompanyOwner
		})
		if p.Actor.MFAVerified != wantMFA {
			t.Errorf("persona %s: MFAVerified = %v", p.Name, p.Actor.MFAVerified)
		}
	}
	// Spot checks of 03 §7.3.
	cm, _ := authztest.Get("club_member")
	if !cm.Actor.HasOrgRole(authztest.ID("club_sensitive"), authz.MemberMember) || !cm.Actor.HasOrgRole(authztest.ID("club_a"), authz.MemberMember) {
		t.Error("club_member must be a member of club_a and club_sensitive")
	}
	ocm, _ := authztest.Get("other_company_member")
	if !ocm.Actor.HasOrgRole(authztest.ID("company_b"), authz.MemberCompanyAdmin) {
		t.Error("other_company_member is company_b's company_admin (03 §1.1)")
	}
	minor, _ := authztest.Get("student_minor")
	if !minor.Actor.IsMinor || !minor.Actor.IsVerifiedStudent() || *minor.Actor.UniversityID != authztest.UniversityID("uni_a") {
		t.Error("student_minor is a verified minor student of uni_a")
	}
	if _, ok := authztest.Get("root"); ok {
		t.Error("Get(root) must fail")
	}
}

func TestResolver_PersonaTokens(t *testing.T) {
	t.Parallel()
	res := authztest.NewResolver(authztest.Personas()...)
	stu, _ := authztest.Get("student")
	cred := httpx.Credential{Transport: httpx.TransportBearer, TokenHash: sha256.Sum256([]byte(stu.Token))}
	ctx, err := res.Resolve(t.Context(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if a, err := authz.ActorFrom(ctx); err != nil || a.UserID != stu.User() || obs.ActorID(ctx) != stu.User().String() {
		t.Fatalf("resolved actor %v, %v", a, err)
	}
	for name, c := range map[string]httpx.Credential{
		"cookie transport": {Transport: httpx.TransportCookie, TokenHash: cred.TokenHash},
		"unknown token":    {Transport: httpx.TransportBearer, TokenHash: sha256.Sum256([]byte("nope"))},
		"empty":            {Transport: httpx.TransportBearer},
	} {
		if _, err := res.Resolve(t.Context(), c); !errors.Is(err, problem.ErrUnauthenticated) {
			t.Errorf("%s: err = %v, want unauthenticated", name, err)
		}
	}
	noMFA := stu.Actor
	res.Add("variant", noMFA)
	if _, err := res.Resolve(t.Context(), httpx.Credential{Transport: httpx.TransportBearer, TokenHash: sha256.Sum256([]byte("variant"))}); err != nil {
		t.Fatalf("added token: %v", err)
	}
}
