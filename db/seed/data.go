package seed

// Invented demo content (05 §13.7): no real person, club, company or
// e-mail domain. Club names are generic (every university has a chess
// club), organisations carry no university prefix, and every description
// says that the row is synthetic.

// clubSpec is one club of the catalogue.
type clubSpec struct {
	Name, Slug, Category, JoinPolicy string
}

// clubCatalog are the non-sensitive clubs; a university takes eight of
// them (rotating). The first one of the main university is the open club
// the smoke test joins.
var clubCatalog = []clubSpec{
	{"Yapay Zekâ Topluluğu", "yapay-zeka", "technology", "open"},
	{"Satranç Topluluğu", "satranc", "hobby", "open"},
	{"Girişimcilik Kulübü", "girisimcilik", "entrepreneurship", "approval"},
	{"Fotoğrafçılık Kulübü", "fotografcilik", "arts", "open"},
	{"Münazara Kulübü", "munazara", "academic", "approval"},
	{"Tiyatro Topluluğu", "tiyatro", "arts", "approval"},
	{"Işık Astronomi Kulübü", "astronomi", "academic", "open"},
	{"Çevre Gönüllüleri", "cevre-gonulluleri", "social_responsibility", "open"},
	{"Müzik Kulübü", "muzik", "arts", "open"},
	{"Dağcılık ve Doğa Sporları Kulübü", "dagcilik", "sports", "approval"},
	{"Endüstri 4.0 Topluluğu", "endustri-40", "technology", "open"},
	{"Sualtı Sporları Kulübü", "sualti", "sports", "approval"},
	{"Kariyer Kulübü", "kariyer", "career", "open"},
}

// sensitiveClubs replace the last club of the first two universities
// (02 §8.5: religious, political and identity clubs are always sensitive).
var sensitiveClubs = []struct {
	clubSpec
	Reason string
}{
	{clubSpec{"İnanç ve Diyalog Topluluğu", "inanc-ve-diyalog", "religious", "approval"}, "religion"},
	{clubSpec{"Toplum ve Siyaset Topluluğu", "toplum-ve-siyaset", "political", "approval"}, "political_opinion"},
}

// companySpec is one demo company.
type companySpec struct {
	Name, Slug, Sector, Size, City string
	Pending                        bool // pending_review (not verified yet)
}

var companyCatalog = []companySpec{
	{"Demo Yazılım A.Ş.", "demo-yazilim", "software", "51-200", "İstanbul", false},
	{"Örnek Enerji Ltd. Şti.", "ornek-enerji", "energy", "201-1000", "Ankara", false},
	{"Deneme Lojistik A.Ş.", "deneme-lojistik", "logistics", "1000+", "İzmir", false},
	{"Taslak Danışmanlık Ltd. Şti.", "taslak-danismanlik", "consulting", "11-50", "Bursa", true},
}

// eventTitles by club category (the first matching entry is used, the
// counter picks among them).
var eventTitles = map[string][]string{
	"technology":            {"Makine Öğrenmesine Giriş Atölyesi", "Açık Kaynak Katkı Günü", "Veri Görselleştirme Atölyesi"},
	"hobby":                 {"Kampüs Satranç Turnuvası", "Hızlı Satranç Akşamı", "Açılış Teorisi Semineri"},
	"entrepreneurship":      {"Fikirden Ürüne Girişim Söyleşisi", "İş Modeli Kanvas Atölyesi", "Yatırımcı Sunumu Provası"},
	"arts":                  {"Sokak Fotoğrafçılığı Yürüyüşü", "Doğaçlama Tiyatro Atölyesi", "Akustik Müzik Gecesi"},
	"academic":              {"Gökyüzü Gözlem Gecesi", "Münazara Hazırlık Kampı", "Bilim Söyleşisi"},
	"social_responsibility": {"Sahil Temizliği Gönüllü Günü", "Fidan Dikim Etkinliği", "Geri Dönüşüm Atölyesi"},
	"sports":                {"Doğa Yürüyüşü", "Tırmanış Duvarı Tanışma Günü", "Yüzme Teknikleri Eğitimi"},
	"career":                {"Özgeçmiş Hazırlama Atölyesi", "Mülakat Simülasyonu", "Mezunlarla Kariyer Sohbeti"},
	"religious":             {"Dinler Arası Diyalog Söyleşisi"},
	"political":             {"Gençlik ve Toplum Paneli"},
	"university":            {"Kariyer Günleri", "Bahar Şenliği Tanıtım Toplantısı", "Yeni Öğrenci Oryantasyonu"},
}

// venues are invented halls.
var venues = []string{
	"Kültür Merkezi A Salonu", "Mühendislik Amfisi 2", "Kütüphane Etkinlik Odası", "Öğrenci Merkezi Çok Amaçlı Salon",
	"Fen Fakültesi Konferans Salonu", "Kampüs Açık Hava Sahnesi",
}

// clubPosts are the texts of club posts ("%s" is the club's name).
var clubPosts = []string{
	"%s olarak yeni dönem tanışma toplantımız bu hafta! Herkesi bekliyoruz.",
	"%s yönetim kurulu seçimleri tamamlandı. Yeni ekibimize başarılar dileriz.",
	"Geçen haftaki etkinliğimize katılan herkese teşekkürler. Fotoğraflar yakında paylaşılacak.",
	"%s gönüllü ekibine katılmak isteyenler için başvurular açıldı.",
	"Dönem planımızı paylaştık: atölyeler, söyleşiler ve kampüs dışı geziler sizi bekliyor.",
	"Ara sınav haftasında etkinliklerimize kısa bir ara veriyoruz. Herkese kolay gelsin!",
	"%s üyelerimiz için hazırladığımız kaynak listesi güncellendi.",
	"Bir sonraki buluşmamızda hangi konuyu konuşalım? Önerilerinizi bekliyoruz.",
}

// companyPosts are the texts of company posts ("%s" is the company).
var companyPosts = []string{
	"%s yaz stajı programı için başvurular yakında açılıyor.",
	"%s ekibi kampüs etkinliklerinde öğrencilerle buluşmaya devam ediyor.",
	"Yeni mezun programımız hakkında bilgi almak için sayfamızı takip edin.",
	"%s mühendislik ekibinden bir günün hikâyesi: kod incelemesi, takım toplantısı ve öğrenme saatleri.",
	"Öğrencilere yönelik ücretsiz eğitim serimizin ilk dersi yayında.",
	"%s olarak kariyer günlerinde standımızı ziyaret eden herkese teşekkürler.",
	"Proje tabanlı staj imkânlarımız hakkında sık sorulan sorular.",
	"Mentorluk programımıza katılan öğrencilerin deneyimlerini paylaştık.",
}

// universityPosts are the texts of university posts.
var universityPosts = []string{
	"Kayıt yenileme takvimi yayımlandı. Ayrıntılar öğrenci işleri sayfasında.",
	"Kariyer merkezimizin danışmanlık saatleri güncellendi.",
	"Kampüs kütüphanesi sınav haftası boyunca 24 saat açık olacak.",
	"Öğrenci topluluklarının dönem etkinlik takvimi yayımlandı.",
}

// platformPosts are Koro's own announcements.
var platformPosts = []string{
	"Koro'ya hoş geldiniz! Kulüplerinizi takip edin, etkinliklere tek tıkla kaydolun.",
	"Etkinlik biletleriniz artık QR kodla e-postanıza geliyor.",
	"Bildirim tercihlerinizi Ayarlar bölümünden yönetebilirsiniz.",
	"Gizliliğiniz bizim için önemli: verilerinizi istediğiniz zaman dışa aktarabilirsiniz.",
	"Bu ortam sentetik demo verisiyle doldurulmuştur; gerçek kişi ve kurumlar içermez.",
}

// expectations are the free-text answers of the demo forms.
var expectations = []string{
	"Yeni insanlarla tanışmak ve konuyu öğrenmek istiyorum.",
	"Uygulamalı deneyim kazanmayı umuyorum.",
	"Kulübü daha yakından tanımak istiyorum.",
	"Kariyerim için faydalı olacağını düşünüyorum.",
	"Arkadaşlarımla birlikte katılmak istiyorum.",
}
