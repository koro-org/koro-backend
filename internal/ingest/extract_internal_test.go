package ingest

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestMask_RemovesEmailPhoneAndNames: addresses and phone numbers by
// pattern, names after titles, in labelled lines and before "ile söyleşi";
// event wording, dates and times stay.
func TestMask_RemovesEmailPhoneAndNames(t *testing.T) {
	t.Parallel()
	in := strings.Join([]string{
		"Bilgi: kulup.baskani@example.org ya da 0 (212) 359 00 00, +90 532 123 45 67",
		"Akademik Danışman : Prof.Dr.Deneme Kişi (Örnek Bölümü)",
		"Konuşmacı: Ayşe Örnekoğlu",
		"Doç. Dr. Ali Veli ile yapay zekâ semineri 12 Kasım 2026 18:00",
		"Ada Lovelace ile Söyleşi — 15.11.2026",
		"Sayın Mehmet Bey katılacak",
		"Speaker: Jane Doe",
		"SİNEMA TOPLULUĞU 11 KASIM 2026 18:00-21:30 FİLM GÖSTERİMİ",
	}, "\n")
	out := maskPersonalData(in)
	for _, leaked := range []string{"kulup.baskani", "example.org", "359 00 00", "532", "Deneme", "Ayşe", "Örnekoğlu",
		"Ali Veli", "Lovelace", "Mehmet", "Jane"} {
		if strings.Contains(out, leaked) {
			t.Errorf("mask kept %q:\n%s", leaked, out)
		}
	}
	for _, kept := range []string{"12 Kasım 2026 18:00", "15.11.2026", "SİNEMA TOPLULUĞU 11 KASIM 2026 18:00-21:30",
		"yapay zekâ semineri", "Söyleşi"} {
		if !strings.Contains(out, kept) {
			t.Errorf("mask lost %q:\n%s", kept, out)
		}
	}
	if got := cleanTitle(maskPerson + " ile Söyleşi"); got != "Söyleşi" {
		t.Errorf("cleanTitle = %q", got)
	}
}

func istanbul(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// TestValidate_EvidenceDateWindowAndYearRule: a candidate whose quote is
// not in the masked text, whose day is not in the quote, whose date is out
// of [yesterday, +180 days] or whose confidence is low is dropped; a date
// without a year is the nearest one not before yesterday; times and ends
// are read in Europe/Istanbul.
func TestValidate_EvidenceDateWindowAndYearRule(t *testing.T) {
	t.Parallel()
	loc := istanbul(t)
	today := time.Date(2026, 12, 20, 10, 0, 0, 0, loc)
	docs := []Document{{URL: "https://kim.example.org/etkinlikler", Text: "BAHAR KONSERİ 12 Ocak 19:30 Kültür Merkezi\n" +
		"KIŞ ŞENLİĞİ 21.12.2026 / YER: KAMPÜS\nESKİ ETKİNLİK 01.03.2026"}}
	ok := Candidate{Doc: 0, Title: "Bahar Konseri", StartDate: "2026-01-12", StartTime: "19:30", YearStated: false,
		Location: "Kültür Merkezi", Category: "culture", Evidence: "BAHAR KONSERİ 12 Ocak 19:30", Confidence: 0.9}
	ev, reason := validateCandidate(ok, docs, today, loc, 0.6)
	if reason != "" {
		t.Fatalf("valid candidate rejected: %s", reason)
	}
	if want := time.Date(2027, 1, 12, 19, 30, 0, 0, loc); !ev.StartsAt.Equal(want) || ev.AllDay || ev.Category != "culture" ||
		ev.Location != "Kültür Merkezi" {
		t.Fatalf("event = %+v, want start %s", ev, want)
	}
	stated := ok
	stated.StartDate, stated.YearStated, stated.StartTime = "2026-12-21", true, ""
	stated.Evidence, stated.Title = "KIŞ ŞENLİĞİ 21.12.2026", "Kış Şenliği"
	if ev, reason := validateCandidate(stated, docs, today, loc, 0.6); reason != "" || !ev.AllDay ||
		!ev.StartsAt.Equal(time.Date(2026, 12, 21, 0, 0, 0, 0, loc)) {
		t.Fatalf("stated year = %+v %s", ev, reason)
	}
	for name, mut := range map[string]func(c *Candidate){
		rejectEvidence: func(c *Candidate) { c.Evidence = "BAHAR KONSERİ 13 Ocak (hallucinated)" },
		"day":          func(c *Candidate) { c.StartDate = "2026-01-14" },
		rejectWindow: func(c *Candidate) {
			c.StartDate, c.YearStated, c.Evidence = "2026-03-01", true, "ESKİ ETKİNLİK 01.03.2026"
		},
		rejectLowConf: func(c *Candidate) { c.Confidence = 0.3 },
		rejectDoc:     func(c *Candidate) { c.Doc = 3 },
		rejectTitle:   func(c *Candidate) { c.Title = maskPerson },
		rejectDate:    func(c *Candidate) { c.StartDate = "12 Ocak" },
	} {
		c := ok
		mut(&c)
		if _, reason := validateCandidate(c, docs, today, loc, 0.6); reason == "" {
			t.Errorf("%s: candidate accepted", name)
		}
	}
	far := ok
	far.StartDate, far.YearStated, far.Evidence = "2027-08-01", true, "BAHAR KONSERİ 12 Ocak"
	if _, reason := validateCandidate(far, docs, today, loc, 0.6); reason == "" {
		t.Error("an event beyond +180 days was accepted")
	}
	masked := ok
	masked.Location, masked.Organizer = maskPerson+" salonu", maskPerson
	if ev, reason := validateCandidate(masked, docs, today, loc, 0.6); reason != "" || ev.Location != "" || ev.Organizer != "" {
		t.Errorf("masked location/organizer kept: %+v %s", ev, reason)
	}
}

// TestFakeExtractor_FindsDatedLines: the development extractor reads the
// date patterns of a page deterministically.
func TestFakeExtractor_FindsDatedLines(t *testing.T) {
	t.Parallel()
	x, err := FakeExtractor{}.Extract(context.Background(), ExtractRequest{
		Today: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC),
		Docs:  []Document{{Text: "SİNEMA TOPLULUĞU\n11 KASIM 2026 18:00-21:30\nSatranç Turnuvası 21.11.2026\nDuyuru"}},
	})
	if err != nil || len(x.Candidates) != 2 {
		t.Fatalf("candidates = %+v %v", x.Candidates, err)
	}
	c := x.Candidates[0]
	if c.StartDate != "2026-11-11" || c.StartTime != "18:00" || c.EndTime != "21:30" || c.Title != "SİNEMA TOPLULUĞU" ||
		!c.YearStated {
		t.Fatalf("first = %+v", c)
	}
	if c := x.Candidates[1]; c.StartDate != "2026-11-21" || c.Title != "Satranç Turnuvası" || c.Evidence != "Satranç Turnuvası 21.11.2026" {
		t.Fatalf("second = %+v", c)
	}
}
