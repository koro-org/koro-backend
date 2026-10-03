// Package fake produces synthetic test and seed data (05 §13.7; KVKK). Real
// personal data never appears in tests, seeds, fixtures or log samples:
//
//   - names, universities, departments and clubs come from the invented or
//     generic lists below (no real person, no real club);
//   - e-mail addresses use only @example.test (reserved TLD, RFC 2606);
//   - phone numbers follow the invalid pattern +90 000 000 00 NN;
//   - national IDs (TCKN) exist only as the fixed, documented vectors of
//     vectors.go, IBANs only as those of internal/platform/iban/ibantest;
//     nothing generates "random valid" ones.
//
// A Faker is deterministic for a seed (math/rand/v2, PCG), so seeds and test
// expectations are reproducible. It is not safe for concurrent use: give
// each goroutine its own.
package fake

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// EmailDomain is the only domain fake addresses use (TestSyntheticOnly
// allows example.test and koro.test).
const EmailDomain = "example.test"

// Faker draws synthetic values from a seeded generator.
type Faker struct {
	r     *rand.Rand
	email int
}

// New returns a Faker for seed.
func New(seed uint64) *Faker {
	return &Faker{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} //nolint:gosec // synthetic test data, not security relevant
}

func (f *Faker) pick(list []string) string { return list[f.r.IntN(len(list))] }

// FirstName returns a common Turkish given name.
func (f *Faker) FirstName() string { return f.pick(firstNames) }

// LastName returns a common Turkish surname.
func (f *Faker) LastName() string { return f.pick(lastNames) }

// FullName returns "<first> <last>".
func (f *Faker) FullName() string { return f.FirstName() + " " + f.LastName() }

// University returns an invented university name.
func (f *Faker) University() string { return f.pick(universities) }

// Department returns a generic department name.
func (f *Faker) Department() string { return f.pick(departments) }

// Club returns an invented student club name.
func (f *Faker) Club() string { return f.pick(clubs) }

// Email returns a unique (per Faker) address at EmailDomain, derived from a
// fake name: "ayse.yilmaz.3@example.test".
func (f *Faker) Email() string {
	f.email++
	local := ASCIIFold(f.FirstName()) + "." + ASCIIFold(f.LastName())
	return fmt.Sprintf("%s.%d@%s", local, f.email, EmailDomain)
}

// Phone returns a number of the invalid pattern +90 000 000 00 NN.
func (f *Faker) Phone() string { return fmt.Sprintf("+90 000 000 00 %02d", f.r.IntN(100)) }

// ASCIIFold lower-cases s with Turkish rules and folds Turkish letters to
// ASCII ("İlkay Çağrı" → "ilkay cagri"); only for building fake addresses.
func ASCIIFold(s string) string {
	return strings.NewReplacer(
		"İ", "i", "I", "i", "ı", "i", "Ç", "c", "ç", "c", "Ğ", "g", "ğ", "g",
		"Ö", "o", "ö", "o", "Ş", "s", "ş", "s", "Ü", "u", "ü", "u", " ", "",
	).Replace(strings.ToLower(strings.NewReplacer("I", "ı", "İ", "i").Replace(s)))
}

// Lists. Names are frequent Turkish given names/surnames (not data about
// anyone); universities and clubs are invented; they deliberately contain
// I/ı/İ/i, Ç, Ğ, Ö, Ş, Ü to exercise Turkish collation and search.
var (
	firstNames = []string{
		"Ayşe", "Fatma", "Emine", "Zeynep", "Elif", "Merve", "Büşra", "Şeyma", "Özge", "İrem",
		"Ilgaz", "Işıl", "Çağla", "Gülşen", "Ümran", "Mehmet", "Mustafa", "Ahmet", "Ali", "Hüseyin",
		"İbrahim", "Emre", "Burak", "Oğuz", "Çağrı", "Görkem", "Ilker", "Şahin", "Ömer", "Uğur",
	}
	lastNames = []string{
		"Yılmaz", "Kaya", "Demir", "Şahin", "Çelik", "Yıldız", "Yıldırım", "Öztürk", "Aydın", "Özdemir",
		"Arslan", "Doğan", "Kılıç", "Aslan", "Çetin", "Kara", "Koç", "Kurt", "Özkan", "Şimşek",
		"Işık", "İnce", "Güneş", "Ünal", "Erdoğan",
	}
	universities = []string{
		"Işıkkent Üniversitesi", "İlıca Teknik Üniversitesi", "Çamlıyamaç Üniversitesi", "Göktaşı Üniversitesi",
		"Şirinköy Üniversitesi", "Ünyeli Bilim Üniversitesi", "Ormanaltı Teknik Üniversitesi", "Dereboyu Üniversitesi",
	}
	departments = []string{
		"Bilgisayar Mühendisliği", "Elektrik-Elektronik Mühendisliği", "Endüstri Mühendisliği", "İktisat",
		"İşletme", "Psikoloji", "Hukuk", "Tıp", "Moleküler Biyoloji ve Genetik", "Mimarlık",
		"Uluslararası İlişkiler", "İstatistik", "Kimya", "Türk Dili ve Edebiyatı",
	}
	clubs = []string{
		"Dağcılık ve Doğa Sporları Kulübü", "Satranç Topluluğu", "Girişimcilik Kulübü", "Yapay Zekâ Topluluğu",
		"Fotoğrafçılık Kulübü", "Münazara Kulübü", "Tiyatro Topluluğu", "Işık Astronomi Kulübü",
		"Çevre Gönüllüleri", "Müzik Kulübü", "Endüstri 4.0 Topluluğu", "Sualtı Sporları Kulübü",
	}
)
