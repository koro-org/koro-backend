package ingest

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"koro/internal/directory"
)

// fixture reads testdata/name as the page of url.
func fixture(t *testing.T, name, url, contentType string) Page {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return Page{URL: url, Status: 200, ContentType: contentType, Body: b}
}

func clubNames(cs []ClubCandidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

// TestAdapters_ClubListFixtures: each pilot list adapter reads its trimmed
// official page: names, the university's category label, the club's site,
// its group address and its profile links; never a person (the advisor
// lines of ODTÜ, the YK of Sabancı), never the page frame.
func TestAdapters_ClubListFixtures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ad := defaultAdapters()

	ytu, err := ad[AdapterClubListYTU].Parse(ctx, Source{}, fixture(t, "ytu_clubs.html",
		"https://www.yildiz.edu.tr/kampus/kampus-yasami/ogrenci-kulupleri", "text/html"))
	if err != nil {
		t.Fatal(err)
	}
	if got := clubNames(ytu.Clubs); !slices.Equal(got, []string{"Alternatif Enerjili Sistemler Kulübü",
		"Bilim ve Matematik Kulübü", "IEEE Kulübü", "Dağcılık Kulübü", "Okçuluk Kulübü", "Sosyal Sorumluluk Kulübü",
		"Yeşilay Kulübü"}) {
		t.Fatalf("ytu names = %q", got)
	}
	if c := ytu.Clubs[0]; c.SourceCategory != "Bilim ve Teknoloji Kulüpleri" {
		t.Fatalf("ytu category = %q", c.SourceCategory)
	}
	if cat, review := clubCategory(ytu.Clubs[3].SourceCategory, ytu.Clubs[3].Name); cat != directory.CategorySports || review {
		t.Fatalf("ytu sports = %s %v", cat, review)
	}

	boun, err := ad[AdapterClubListBOUN].Parse(ctx, Source{}, fixture(t, "boun_clubs.html",
		"https://kultursanat.bogazici.edu.tr/tr/pages/kulupler-ve-topluluklar/2482", "text/html"))
	if err != nil {
		t.Fatal(err)
	}
	if got := clubNames(boun.Clubs); !slices.Equal(got, []string{"Bilim Kulübü", "Bilişim Kulübü", "Briç Kulübü",
		"Din ve İnanç Araştırmaları Kulübü", "Doğa Sporları Kulübü"}) {
		t.Fatalf("boun names = %q", got)
	}
	bilisim := boun.Clubs[1]
	if bilisim.Abbr != "COMPEC" || bilisim.Website != "https://compec.example.com/" || bilisim.Email != "compec@example.org" {
		t.Fatalf("boun bilişim = %+v", bilisim)
	}
	if bric := boun.Clubs[2]; bric.Website != "" || bric.Social["instagram"] == "" {
		t.Fatalf("an Instagram 'site' must be a profile link: %+v", bric)
	}
	if cat, review := clubCategory("", boun.Clubs[3].Name); cat != directory.CategoryOther || !review {
		t.Fatalf("a religious name = %s %v, want other + review", cat, review)
	}
	if got := clubDisplayName(bilisim); got != "Bilişim Kulübü (COMPEC)" {
		t.Fatalf("display name = %q", got)
	}

	su, err := ad[AdapterClubListSU].Parse(ctx, Source{URL: "https://studentlife.sabanciuniv.edu/kulupler?c=5"},
		fixture(t, "sabanci_clubs.html", "https://studentlife.sabanciuniv.edu/kulupler?c=5", "text/html"))
	if err != nil {
		t.Fatal(err)
	}
	if got := clubNames(su.Clubs); !slices.Equal(got, []string{"Astronomi Kulübü (ASTROSU)", "Bilgisayar Kulübü (SU CSS)",
		"Robotik Kulübü"}) {
		t.Fatalf("sabanci names = %q", got)
	}
	if c := su.Clubs[0]; c.SourceCategory != "Fen ve Teknoloji" || c.Social["instagram"] == "" || c.Social["x"] == "" {
		t.Fatalf("sabanci astronomi = %+v", c)
	}
	if c := su.Clubs[1]; c.Social["instagram"] != "" || c.Social["youtube"] == "" {
		t.Fatalf("a login-wall Instagram link must be dropped: %+v", c)
	}

	metu, err := ad[AdapterClubListMETU].Parse(ctx, Source{}, fixture(t, "metu_clubs.html",
		"https://kim.metu.edu.tr/tr/ogrenci-topluluklari", "text/html"))
	if err != nil {
		t.Fatal(err)
	}
	if got := clubNames(metu.Clubs); !slices.Equal(got, []string{"Amatör Fotoğrafçılık Topluluğu", "Çağdaş Dans Topluluğu",
		"Bilim ve Teknik Topluluğu"}) {
		t.Fatalf("metu names = %q", got)
	}
	if c := metu.Clubs[0]; c.Abbr != "AFT" || c.Website != "http://aft.example.edu.tr/" {
		t.Fatalf("metu aft = %+v", c)
	}
	for _, c := range metu.Clubs {
		for _, v := range append([]string{c.Name, c.Abbr, c.Website, c.Email}, c.SourceCategory) {
			if strings.Contains(v, "Deneme") || strings.Contains(v, "Örnek") {
				t.Fatalf("an advisor was read: %+v", c)
			}
		}
	}
	if httpsSite(metu.Clubs[0].Website) != "" || httpsSite(metu.Clubs[1].Website) == "" {
		t.Fatal("only an https site goes to the org profile")
	}
}

// TestAdapters_EventFixtures: the events page's main text (no navigation,
// no footer contacts), the JSON list's items as key lines (no image), the
// feed's items with their links, the sitemap's pages on its own host.
func TestAdapters_EventFixtures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ad := defaultAdapters()

	metu, err := ad[AdapterHTMLPage].Parse(ctx, Source{}, fixture(t, "metu_events.html", "https://kim.metu.edu.tr/tr/etkinlikler", "text/html"))
	if err != nil || len(metu.Docs) != 1 {
		t.Fatalf("metu = %+v %v", metu, err)
	}
	text := metu.Docs[0].Text
	if !strings.Contains(text, `11 KASIM 2026 18:00-21:30 / ETKİNLİK: "FİLM GÖSTERİMİ" / YER: FİZİK U-2`) ||
		strings.Contains(text, "Anasayfa") || strings.Contains(text, "kim@example.org") {
		t.Fatalf("metu text = %q", text)
	}

	itu, err := ad[AdapterJSONAPI].Parse(ctx, Source{}, fixture(t, "itu_events.json",
		"https://etkinlik.itu.edu.tr/public/embed/etkinlikler.aspx", "application/json"))
	if err != nil || len(itu.Docs) != 3 {
		t.Fatalf("itu = %d docs %v", len(itu.Docs), err)
	}
	first := itu.Docs[0].Text
	if !strings.Contains(first, "EtkinlikAdı: Aikido Gösterisi/Semineri") || !strings.Contains(first, "EtkinlikBaslangicTarihi: 2026-11-20T16:00:00") ||
		strings.Contains(first, "uploads") {
		t.Fatalf("itu item = %q", first)
	}
	if strings.Contains(itu.Docs[1].Text, "<p>") {
		t.Fatalf("html left in a JSON field: %q", itu.Docs[1].Text)
	}
	if masked := maskPersonalData(first); strings.Contains(masked, "Deneme") || strings.Contains(masked, "aikido@") {
		t.Fatalf("itu item not masked: %q", masked)
	}

	feed, err := ad[AdapterRSS].Parse(ctx, Source{}, fixture(t, "feed.rss", "https://club.example.org/feed", "application/rss+xml"))
	if err != nil || len(feed.Docs) != 2 {
		t.Fatalf("rss = %+v %v", feed, err)
	}
	if feed.Docs[0].URL != "https://club.example.org/etkinlik/bahar" || !strings.Contains(feed.Docs[0].Text, "12 Mayıs 2027") ||
		strings.Contains(feed.Docs[0].Text, "<p>") {
		t.Fatalf("rss item = %+v", feed.Docs[0])
	}

	sm, err := ad[AdapterSitemap].Parse(ctx, Source{Host: "takvim.bogazici.edu.tr"}, fixture(t, "boun_takvim_sitemap.xml",
		"https://takvim.bogazici.edu.tr/tr/site/events", "text/xml"))
	if err != nil || len(sm.Links) != 2 {
		t.Fatalf("sitemap = %+v %v", sm, err)
	}
	if sm.Links[0].URL != "https://takvim.bogazici.edu.tr/tr/events/kis-konseri/16590" || sm.Links[0].LastMod == nil {
		t.Fatalf("sitemap order = %+v", sm.Links)
	}

	if _, err := ad[AdapterManualPaste].Parse(ctx, Source{}, Page{}); err == nil || ad[AdapterManualPaste].Accept() != nil {
		t.Fatal("manual_paste must never fetch")
	}
	if _, err := ad[AdapterJSONAPI].Parse(ctx, Source{}, Page{Body: []byte("not json")}); err == nil {
		t.Fatal("a broken JSON body was read")
	}
}

// TestNormalize_CategoriesAndSensitiveNames: the pilot labels map
// deterministically; a special category is never a target; a name that
// hints at one gives other + review whatever the label.
func TestNormalize_CategoriesAndSensitiveNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		label, name string
		cat         directory.Category
		review      bool
	}{
		{"Bilim ve Teknoloji Kulüpleri — 30 Kulüp", "Robotik Kulübü", directory.CategoryTechnology, false},
		{"Kariyer Kulüpleri", "Finans Kulübü", directory.CategoryCareer, false},
		{"Sanat - Performans ve Eğlence", "Tiyatro", directory.CategoryArts, false},
		{"Sosyal Bilimler Kulüpleri", "Tarih Kulübü", directory.CategoryAcademic, true},
		{"Toplumsal Gelişim Kulüpleri", "Atatürkçü Düşünce Kulübü", directory.CategoryOther, true},
		{"Hobi", "LGBTİ+ Dayanışma", directory.CategoryOther, true},
		{"", "Satranç Kulübü", directory.CategoryOther, true},
		{"Bilinmeyen Etiket", "Dans Kulübü", directory.CategoryOther, true},
	} {
		label := countSuffixRe.ReplaceAllString(tc.label, "")
		cat, review := clubCategory(label, tc.name)
		if cat != tc.cat || review != tc.review || cat.Sensitive() {
			t.Errorf("clubCategory(%q, %q) = %s %v, want %s %v", label, tc.name, cat, review, tc.cat, tc.review)
		}
	}
	if d, ok := emailDomain("Compec@Example.ORG"); !ok || d != "example.org" {
		t.Errorf("emailDomain = %q %v", d, ok)
	}
	if _, ok := emailDomain("not an address"); ok {
		t.Error("a malformed address passed")
	}
}
