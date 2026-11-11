package directory_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
)

func TestClubNameKey_FoldsListSpellings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ a, b string }{
		{"Bilişim Kulübü (COMPEC)", "BİLİŞİM TOPLULUĞU"},
		{"ÇAĞDAŞ DANS TOPLULUĞU (ÇDT)", "Çağdaş Dans Kulübü"},
		{"IEEE Kulübü", "IEEE Student Club"},
	} {
		if ka, kb := directory.ClubNameKey(tc.a), directory.ClubNameKey(tc.b); ka != kb || ka == "" {
			t.Errorf("ClubNameKey(%q) = %q, ClubNameKey(%q) = %q", tc.a, ka, tc.b, kb)
		}
	}
	if k := directory.ClubNameKey("Kulüp"); k != "kulup" {
		t.Errorf("noise-only name key = %q", k)
	}
	if directory.ClubNameKey("Satranç Kulübü") == directory.ClubNameKey("Dans Kulübü") {
		t.Error("different clubs share a key")
	}
}

// TestImportClub_CreatesUnclaimedOnceAndMatchesExisting: the first import
// opens an unclaimed, non-sensitive club in the caller's transaction (org,
// row, system audit, club.updated); the same club under another spelling
// and an admin-created club are matched, not duplicated.
func TestImportClub_CreatesUnclaimedOnceAndMatchesExisting(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-IMP", "Ege Üniversitesi", "İzmir")
	admin := e.club(t, uni, "Satranç Kulübü", directory.CategoryHobby)
	before := len(e.out.Events())

	id, created, err := e.svc.ImportClub(ctx, directory.ImportedClub{
		UniversityID: uni.ID, Name: "Bilişim Kulübü (COMPEC)", Category: directory.CategoryTechnology,
		WebsiteURL: "https://compec.example.org", SocialLinks: map[string]string{"instagram": "https://instagram.com/x"},
		Job: "ingest.fetch_due",
	})
	if err != nil || !created {
		t.Fatalf("ImportClub = %v %v", created, err)
	}
	c, err := e.mem.GetClub(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.ClaimStatus != directory.ClaimUnclaimed || c.IsSensitive || c.JoinPolicy != directory.JoinApproval ||
		c.Category != directory.CategoryTechnology || c.SocialLinks["instagram"] == "" {
		t.Fatalf("club = %+v", c)
	}
	if n := len(e.out.Events()); n != before+1 || e.out.Events()[n-1].Topic != domainevent.TopicClubUpdated {
		t.Fatalf("events = %+v", e.out.Events())
	}
	ev := e.audit.Of(authz.DirectoryClubCreate)
	if last := ev[len(ev)-1]; last.SystemJob != "ingest.fetch_due" || last.ActorUserID != nil {
		t.Fatalf("audit = %+v", last)
	}

	again, created, err := e.svc.ImportClub(ctx, directory.ImportedClub{
		UniversityID: uni.ID, Name: "BİLİŞİM TOPLULUĞU", Category: directory.CategoryOther, Job: "ingest.fetch_due",
	})
	if err != nil || created || again != id {
		t.Fatalf("second spelling = %v %v %v, want %v", again, created, err, id)
	}
	if got, created, err := e.svc.ImportClub(ctx, directory.ImportedClub{
		UniversityID: uni.ID, Name: "SATRANÇ TOPLULUĞU", Category: directory.CategoryOther, Job: "ingest.fetch_due",
	}); err != nil || created || got != admin.Club.OrgID {
		t.Fatalf("admin club = %v %v %v", got, created, err)
	}

	for _, in := range []directory.ImportedClub{
		{UniversityID: uni.ID, Name: "Din Kulübü", Category: directory.CategoryReligious},
		{UniversityID: uuid.NewV7(), Name: "Yeni Kulüp", Category: directory.CategoryArts},
		{UniversityID: uni.ID, Name: "X", Category: directory.CategoryArts},
		{UniversityID: uni.ID, Name: "Link Kulübü", Category: directory.CategoryArts, SocialLinks: map[string]string{"x": "http://x.example.org"}},
	} {
		in.Job = "ingest.fetch_due"
		if _, _, err := e.svc.ImportClub(ctx, in); err == nil {
			t.Errorf("ImportClub(%q) accepted", in.Name)
		} else if s, _ := problemtest.StatusCode(err); s != 422 {
			t.Errorf("ImportClub(%q) = %v, want 422", in.Name, err)
		}
	}
}
