# koro-backend — agent talimatı

Bu repo Koro'nun Go backend'idir (tek modül `koro`, tek binary `cmd/koro`). Ticket'lar ve tasarım belgeleri
**repo dışındadır**: `koro-files/tickets/` ve `koro-files/docs/` (bu reponun üst klasörü; bir worktree'den
`../../../tickets`, `../../../docs`).

## Başlamadan önce oku (sırayla)
1. Kendi ticket'ın: `koro-files/tickets/BE-NNN-*.md` — kapsam, kabul kriterleri, test adları.
2. `koro-files/docs/design/06-hizalama-kararlari.md` — **bağlayıcı**, tamamı. Çelişkide 06 > diğerleri.
3. Ticket'taki **"Okunacak bölümler"** — yalnız o bölümler (`grep -n "^#" <dosya>` ile başlığı bul; 01–05 çok büyük,
   tamamını okuma). Kaynak önceliği (06 H-11): yetki → 03 · uç/sözleşme → 04 · şema → 02 · kod kalıbı → 05 · modül sınırı → 01.
4. `koro-files/tickets/README.md` sonundaki "Ortak Definition of Done".

## Komutlar
- `make help` hedefleri listeler. `go test` yerine `make test` (aynı bayraklar: `-race -shuffle=on`).
- `make lint` (golangci-lint v2, config verify dahil) · `make test` · `make build` · `make openapi` + `make openapi-check`
  · `make tidy-check` · `make sec` · `make check` (PR öncesi tamamı) · `make test-integration` (Docker).
- Araçlar (golangci-lint, govulncheck) **Go 1.27 ile derlenmiş** olarak repo içi `bin/` klasörüne kurulur (`make tools`;
  `bin/` gitignore'da). Global kurulum yapma.
- `go.mod`: `go 1.27.2`; `GOTOOLCHAIN=auto` ile 1.27.2 kendiliğinden iner (OPS-002: govulncheck GO-2026-6599…6617). Makefile `GO_TOOLCHAIN` (yalnız araç derlemesi) da go1.27.2 (OPS-001).

## Demo verisi (BE-033; yalnız dev/staging)
- `make seed-demo` hedefi yok (Makefile dondurulmuş, R9). `make dev` (bağımlılıklar + migration + api + worker) açıkken başka
  bir terminalde, `.env` yüklüyken (`set -a; . ./.env; set +a`):
  `go run ./cmd/koro seed reference` → `go run ./cmd/koro seed demo -password '<en az 15 karakter>'`.
- `-password` yalnız öğrenci, kulüp, şirket ve temsilci hesaplarına yazılır; platform rolleri (admin, finans, DPO, moderatör,
  destek) parola sıfırlama ve TOTP ile açılır (06 H-66).
- `seed demo` `APP_ENV=prod`'da (ve dev/staging dışındaki her değerde) veritabanına dokunmadan reddeder; idempotenttir
  (sabit kimlikler + `ON CONFLICT DO NOTHING`, ikinci koşu 0 satır ekler). Veri tamamen sentetiktir: `@example.test`
  adresleri, `testutil/fake` adları, telefon/TCKN/IBAN yok; outbox satırı yazmaz (e-posta/bildirim gitmez).
- İçerik (`db/seed`): 3 üniversite (referans veriden), 24 kulüp (2 hassas, 1 sahipsiz), 300 öğrenci (12'si 18 yaş altı),
  4 şirket (1'i `pending_review`), 34 etkinlik (geçmiş/dolu/bekleme listeli/başvurulu/üniversiteye özel/kayıtsız/taslak/
  iptal), ~370 kayıt + bilet + check-in, ~220 gönderi + beğeni, segmentler, `targeted_post` hakları, 2 platform admini,
  2 finans admini, DPO, moderatör, destek. Bilinen hesaplar komut çıktısında ve `db/seed/seed.go` sabitlerinde
  (`demo.yonetici1@example.test`, `demo.kulup@example.test`, `demo.ogrenci@example.test`, `demo.sirket@example.test` …).
- Uçtan uca duman testi: `cmd/koro/smoke_integration_test.go` (`TestSmoke_ClubFirstPilotFlow`, `make test-integration`).

## Kod kuralları (özet; ayrıntı 05)
- Kodda her şey İngilizce (identifier, yorum, hata/log mesajı, OpenAPI özeti); commit özeti Türkçe.
- Hata: `internal/platform/problem` (H-14). Sentinel `problem.New(kind, "<modül>.<kod>", "Title")` modülün `errors.go`'sunda;
  handler hatayı olduğu gibi döndürür, RFC 9457'ye çeviri yalnız `httpx`'te.
- Uç kaydı: `huma.Register(app.V1, op.Koro(huma.Operation{...}, op.Meta{Action, Surface, ...}), h)` (`internal/platform/httpx/op`).
- Config: yalnız `internal/platform/config/<bölüm>.go` (kendi bölümün) + `.env.example`'daki `# --- <bölüm> ---` bloğun.
- Kompozisyon: yalnız `cmd/koro/wire_<modül>.go` ve `cmd/koro/wire_<modül>_test.go` (kalıp `cmd/koro/wire.go` başındaki yorumda).
- `time.Now()` yok (`clock.Clock`), `os.Getenv` yok (`config`), PII loglanmaz (`obs` maskeleri), testler `t.Parallel()`,
  yalnız stdlib `testing` (testify yok), kimlik stdlib `uuid.NewV7()`.

## Paralel çalışma kuralları (BE-001 R1–R11; tüm BE ticket'larını bağlar)
> Bu kurallardan 05/03 ile çelişenler `docs/design/00-acik-sorular-tasarim.md` §F'de (T-71…T-77) kayıtlıdır; karar gelene kadar burası geçerlidir.

- **R1 — Worktree ve dal.** Her ticket `git -C koro-backend worktree add .worktrees/BE-NNN -b be-NNN-<slug> main` ile açılan worktree'de yapılır. Ticket/karar dosyaları (`koro-files/tickets`, `koro-files/docs`) repo dışındadır: agent yalnız **kendi ticket dosyasını** düzenler, `DECISIONS-LOG.md`'ye yalnız sona satır ekler; `INDEX.md`'yi orkestratör günceller.
- **R2 — Migration sahipliği (önceden tahsis, 02 §23 numaraları).** `000001–000009` BE-002 · `000010–000043` BE-009 · `000044–000064` BE-010 · `000065–000093` BE-011 · `000094–000120` BE-034 · `000121` BE-049. Modül kod ticket'ları yeni migration **açmaz**. Pilot öncesi (repo'da `db/migrations/.frozen` yokken) şema düzeltmesi gerekirse ilgili dosya yerinde düzeltilir, 02 aynı ticket'ta güncellenir (06 H-20) ve ticket notuna yazılır. Pilot dağıtımında orkestratör `.frozen` dosyasına dağıtılan sürümü yazar: bundan sonra ≤ taban dosyalar değişmez, yeni dosya merge anında `max(main)+1` numarasını alır (05 §7.1 yeniden numaralama kuralı).
- **R3 — Yetki matrisi modül başına.** `cmd/koro/testdata/authz/<modül>.yaml` (yalnız o modülün tag'ine ait operasyonlar), `_personas.yaml` (18 persona; BE-004 yazar, sonra dondurulur), `fixtures/<modül>.yaml`, `scenarios/<modül>.yaml`, `bodies/<modül>/*.json`; fixture kurulum kodu `cmd/koro/matrix_<modül>_test.go`. Aynı operationId iki dosyada = test hatası.
- **R4 — Bağlama modül başına.** `cmd/koro/wire.go` (+ `container`) BE-001'de yazılır; P0 içinde yalnız BE-002 (`infra.db`, readyz DB kontrolü), BE-004 (`hooks.memberships`, test ActorResolver bağlantısı) ve BE-006 (`infra.outbox/queue/limiter`, worker kaydı) ekleme yapar; **P0 bitince dondurulur.** Her modül yalnız `cmd/koro/wire_<modül>.go` (GORM store, `New(Deps)`, `Register`, abonelikler, işler, `privacy.Provider`/süpürücü kaydı, kanca doldurma) ve `cmd/koro/wire_<modül>_test.go` (bellek içi store) dosyalarını düzenler. 01 §5.3'teki `func` enjeksiyonları ve sonradan gelen sağlayıcılar `container.hooks` alanlarıyla geç bağlanır: sağlayıcı ticket kancayı doldurur; tüketici `nil` kancada güvenli varsayılan döner. Kancalar yalnız stdlib, `platform/*` ve `authz` tipleriyle tanımlanır (modül tipleri kancaya girmez; tüketici kendi portuna uyarlar). BE-001'in ön-tanımladığı kancalar: `domainResolver` (directory→identity), `clubInfo` (directory→orgs `joinClub`), `anonResolver` (discovery→messaging), `collectionAccount` (payments→orders), `openCommerce` (liste; orders ve ledger açık sipariş/payout kontrolü ekler → orgs `closeOrg`), `talentAuditor` (talent→kvkk; rapor JSON + nötrleme fonksiyonları); ayrıca `privacy` (`[]privacy.Provider`) ve `sweepers` (`[]privacy.RetentionSweeper`) listeleri (kvkk okur). BE-004: `memberships` (orgs→Actor).
- **R5 — authz sabitleri önceden üretilir.** BE-004 03 §5 + §3.7'deki **tüm** action sabitlerini `internal/authz/actions_<modül>.go` dosyalarına ve her modül için "hepsini reddet" `policy_<modül>.go` stub'ını yazar. Modül ticket'ı yalnız kendi `policy_<modül>.go` + testini değiştirir. Yeni action = önce 03 değişir (orkestratör).
- **R6 — Olay sözleşmeleri önceden üretilir.** BE-006 01 §6.2'deki tüm topic sabitlerini ve payload struct'larını `internal/platform/domainevent/<modül>.go` dosyalarına yazar; modül ticket'ı yalnız kendi dosyasına (gerekirse) alan ekler.
- **R7 — Config ve `.env.example` modül başına.** `internal/platform/config/<modül>.go` (BE-001'de boş struct); `.env.example`'da modül başına `# --- <modül> ---` başlıklı, aralarında boş satır bulunan bölümler. Modül ticket'ı yalnız kendi bölümünü düzenler.
- **R8 — Bildirim şablonları.** `internal/notify/templates/<modül>/...` altına yalnız **yeni dosya** eklenir (notify paketinin koduna dokunulmaz).
- **R9 — Üretilen ve ortak dosyalar.** `openapi/*` elle birleştirilmez: rebase sonrası `make openapi` yeniden üretilir. `go.mod/go.sum` çakışmasında main'inki alınır + `go mod tidy`. `Makefile`, `.golangci.yml`, `wire.go`, `internal/arch/**`, `internal/authz/{actor,can,...}.go` (çekirdek), `internal/platform/**` P0'dan sonra yalnız ticket'ın "Kapsam"ında açıkça yazıyorsa değiştirilir; değilse ayrı küçük ticket açılır.
- **R10 — Aynı modül = sıralı.** Aynı modül paketine yazan iki ticket aynı dalgada çalışmaz (`depends_on` zinciri). Paralel ticket'lar ortak modül paketi paylaşmaz.
- **R11 — Merge.** Orkestratör: worktree'de `git rebase main` → üretilen dosyaları yeniden üret → `make check && make test-integration` → `git merge --ff-only` → `git worktree remove`. Ticket başına tek commit (`BE-NNN: özet`).

## Bitirirken
- Kabul kriterlerindeki testler **adlarıyla** var ve geçiyor; `make lint test build openapi-check` yeşil.
- Ticket checklist'i işaretli, "İlerleme günlüğü" güncel, `status: review`. Sormadan alınan kararlar ticket'ın
  "Notlar / kararlar" bölümüne **ve** `docs/DECISIONS-LOG.md` sonuna (yalnız ekleme).
- Tasarımda hata/eksik: 01–05'i değiştirme (06 H-22 istisnası hariç); ticket notuna ve `docs/design/00-acik-sorular-tasarim.md` sonuna yeni T-numarasıyla yaz.
- Tek commit: `BE-NNN: <Türkçe emir kipi özet>` + son satır `Co-Authored-By: Claude <Model> <noreply@anthropic.com>` (06 H-23).
  Dosyaları adıyla ekle (`git add -A` yok); commit öncesi `git status` ile `bin/`, `.env`, coverage sızmadığını kontrol et.
