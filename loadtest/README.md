# Yük testi (OPS-005)

Pilot akışlarının k6 senaryoları ve onları GitHub Actions'ta koşturan elle tetiklenen iş
(`.github/workflows/loadtest.yml`). Test, gerçek `koro api` ve `koro worker` süreçlerini PostgreSQL 18, Redis 8,
SeaweedFS (S3) ve Mailpit ile `docker compose` içinde, sentetik demo verisinin (`koro seed demo`) üzerinde ölçer.
PR'da ya da push'ta **koşmaz**. Geliştirici makinesinde de koşturulmaz (bütün yığın + k6 birkaç çekirdeği dakikalarca
doldurur).

| Dosya | Ne |
|---|---|
| `compose.yaml` | Test ortamı: altyapı + `deploy/Dockerfile` imajıyla api/worker; tek seferlik kurulum adımları (`setup` profili) |
| `prep.sql` | Seed sonrası hazırlık (yalnız yönetici yüzeyinin yapacağı iki şey; aşağıda) + `pg_stat_statements` |
| `students.sql` | k6'nın oturum açtığı demo öğrencilerin listesi (JSON) |
| `k6/main.js` | k6 giriş noktası: seçenekler, `setup`, senaryolar, `handleSummary` |
| `k6/lib/config.js` | Profiller, senaryo seçimi, eşikler |
| `k6/lib/setup.js` | Senaryoların ortak hazırlığı (gerçek API üzerinden) |
| `k6/lib/scenarios.js` | Senaryo fonksiyonları |
| `k6/lib/api.js`, `totp.js`, `summary.js` | HTTP yardımcıları, RFC 6238 TOTP, Markdown özeti |

## Nasıl tetiklenir

GitHub → **Actions → loadtest → Run workflow** (yalnız `main` ya da bir dal seçilerek). Komut satırından:

```bash
gh workflow run loadtest.yml -R koro-org/koro-backend -f profile=pilot
gh workflow run loadtest.yml -R koro-org/koro-backend -f profile=smoke -f scenarios=checkin,orders
gh workflow run loadtest.yml -R koro-org/koro-backend -f profile=stress -f rate_limits=true
gh run watch -R koro-org/koro-backend    # son koşuyu izle
```

| Girdi | Varsayılan | Ne |
|---|---|---|
| `profile` | `pilot` | `smoke` (~2 dk, betik doğrulaması) · `pilot` (2 dk artış + 10 dk kararlı + 30 sn iniş) · `soak` (30 dk kararlı, raporun dayanıklılık testi) · `stress` (5 basamakta pilot yükünün ~2,5 katına, dirsek noktasını bulmak için) |
| `scenarios` | `all` | Virgüllü liste: `browse,signup,register,checkin,orders,discovery` |
| `vus` | profilinki | `browse`'un tepe VU sayısı (1–5000) |
| `duration` | profilinki | Kararlı evrenin süresi (`90s`, `5m` …; `stress`'te basamak başına) |
| `rate_limits` | `false` | `true`: API `RATE_LIMIT_ENABLED=true` ile açılır (aşağıda) |

İş süresi: imaj derlemesi + ortamın kurulması ~4–6 dk, `setup` ~1 dk, üstüne profilin süresi; zaman aşımı 90 dk.
Aynı anda tek koşu (`concurrency: loadtest`). koro-backend public olduğundan standart runner dakikası ücretsizdir;
repo private'a dönerse `pilot` bir koşu ~20 dk harcar.

## Senaryolar

Karışım okuma ağırlıklıdır; okuma kapalı modeldir (düşünme süreli VU'lar), yazma akışları açık modeldir (saniyedeki
varış hızı sabit: API yavaşlayınca sunulan yük azalmaz). Pilot ölçeği: 2–3 üniversite, 10–20 kulüp, birkaç bin
öğrenci (Teknik Rapor §8.5'in "500 eşzamanlı kullanıcı" hedefi).

| Senaryo | Akış | Pilot | Gerekçe |
|---|---|---|---|
| `browse` | Öğrenci: akış %35 (¼'ünde 2. sayfa), etkinlik listesi %20 (filtreli/sayfalı), etkinlik detayı %20, kayıtlarım %10, kulüpler %8, `/me` %7. Her beşinci VU oturumsuz ziyaretçi: yalnız etkinlik listesi/detayı. Düşünme süresi 3–7 sn. | 400 VU (~80 istek/sn) | Tepe saatte ~500 eşzamanlı kullanıcının çoğu okur |
| `signup` | Kayıt (`POST /auth/register`) → doğrulama e-postası Mailpit'ten → `email-verifications/confirm` → giriş → `/me` | 1/sn | Açılış günü ~1000 kayıt, tepede ~1/sn |
| `register` | Öğrenci etkinliğe kayıt olur (`Idempotency-Key`), %30'u biletini açar | 5/sn | Popüler bir etkinlik duyurusu: 300 koltuk ~1 dakikada |
| `checkin` | Görevli (event staff, `scanner`) QR okutur; %20'sinde kapı sayacı (`check-ins/stats`) | 2/sn | 300 kişi 3 kapıdan ~3 dakikada |
| `orders` | Şirket sahibi (TOTP'li oturum) sipariş verir → havale talimatı (referans = sipariş no, IBAN) → sipariş detayı → iptal | 0,2/sn | Seyrek; yol çalışsın diye sıfırın üstünde |
| `discovery` | İK (recruiter, %70) ve şirket sahibi keşif araması (üniversite/sınıf filtresi), %30'unda takma adlı profil | 1/sn | Birkaç işe alımcı sayfa çeviriyor |

Toplam ~500 VU (400 `browse` + açık modellerin uçuştaki VU'ları). `stress` her hızı ×5, `browse`'u 1000 VU'ya çıkarır.

### Ortak hazırlık (`k6/lib/setup.js`, gerçek API)

- `students.sql` listesindeki demo öğrencilerin (18+, doğrulaması etkin, ~280) hepsi mobil istemci olarak oturum açar
  (`X-Koro-Client: mobile`, Bearer token; cookie olmadığı için CSRF yok). Kulüp yöneticisi `demo.kulup`, şirket sahibi
  `demo.sirket` (TOTP kaydı + onayı: sipariş `company_admin` için MFA ister), İK `demo.ik` (keşif, MFA gerekmez).
- Ana kulübe kapasitesi 100 000 olan kayıt etkinlikleri: sayıları, koşunun bütün kayıtları farklı (öğrenci, etkinlik)
  çiftine düşecek kadar (`hız × süre / öğrenci + 1`, en çok 60). Çiftler biterse yanıt `409 events.already_registered`
  olur; beklenen yanıt sayılır, `koro_registration_repeats`'te görünür.
- Bir kapı etkinliği: 3 öğrenci `scanner` görevli yapılır, ~250 öğrenci kayıt olup biletini (QR) alır. Bütün biletler
  okutulunca kapı tekrar okutmaya geçer (`already_checked_in`, 200).
- 60 öğrenci keşfe açılır (`PATCH /me/privacy`, `discovery_profile` açık rıza metniyle; ayar ve rıza kaydı uygulamada
  aynı işlemde).
- Siparişler demo seed'in Model A havale tekliflerinden (`demo_targeted_post`, `demo_club_sponsorship`; 9'ar adet);
  her sipariş hemen iptal edildiği için adet teklife döner. Örtüşen siparişlerde `409 catalog.sold_out` beklenen yanıttır
  (`koro_orders_sold_out`). Hizmet kodu yetmez: Model C doğrudan teklifi (`db/seed/commerce_direct.go`) de
  `demo_club_sponsorship` satar ve onaylı banka hesabı olmayan demo payee yüzünden detay/talimat
  `409 parties.no_approved_account` döner. Teklif kartı modeli göstermediği için setup her aday teklife bir yoklama
  siparişi verip hemen iptal eder ve yalnız `model_code: A` olanları kullanır.

### `prep.sql` neden var

Demo seed iki şeyi içermiyor ve ikisi de normalde yönetici yüzeyinden (parola sıfırlama + TOTP + dört göz) gelir:
`discovery_profile` için yayımlanmış bir açık rıza metni (öğrencinin keşfe açılması bunu ister) ve demo şirketine
`discovery_access` hakkı (keşif araması bunu ister). Bu iki satır test veritabanına doğrudan yazılır; geri kalan her şey
(rıza, kayıt, sipariş …) API'den geçer. Üretim kodu değişmez.

## Rate limit

Bütün k6 trafiği tek makineden gelir; üretimdeki IP başına sınırlar (genel 300/dk, giriş 10/dk, kayıt 5/saat …,
`internal/platform/ratelimit`) açıkken test birkaç saniyede 429'a döner. Bu yüzden:

- **Varsayılan (`rate_limits=false`):** API `RATE_LIMIT_ENABLED=false` ile açılır (yalnız `APP_ENV=dev`'de izinli;
  `config/ratelimit.go`). Uygulamanın kapasitesi ölçülür; sınırlayıcının Redis çağrısı ölçüme girmez.
- **`rate_limits=true`:** sınırlar açık. Compose ağı `TRUSTED_PROXIES`'te olduğundan API istemci IP'sini
  `X-Forwarded-For`'dan alır; k6 her sanal kullanıcıya RFC 2544 aralığından (`198.18.0.0/15`) ayrı bir IP verir
  (kayıtta her denemeye ayrı IP: `auth_register` 5/saat/IP). Üretimdeki Traefik arkası durumun benzeridir. Aktör başına
  sınırlar (okuma 600/dk, yazma 120/dk, QR 120/dk, arama 60/dk) gerçek değerleriyle çalışır; 429 bu kipte beklenen yanıt
  sayılır ve `koro_rate_limited`'te görünür (`stress`'te görevli başına QR sınırı aşılır: beklenen).

İki kip de yalnız ortam değişkeniyle seçilir; sınır değerleri koddaki sabitlerdir (`ratelimit.Rules`).

## Eşikler ve gerekçesi

Kaynak: Teknik Rapor §8.5 ("500 eşzamanlı kullanıcıda okuma p95 < 300 ms, yazma p95 < 800 ms, hata < %1"). Ölçüm, bütün
yığının ve k6'nın tek 4 vCPU'luk runner'ı paylaştığı kötümser bir düzende yapılır; eşik bu düzende geçiyorsa pilot
sunucularında (ayrı makineler) pay vardır.

| Metrik | Eşik | Neden |
|---|---|---|
| `http_req_failed` | < %1 | Beklenmeyen her durum kodu (5xx, yanlış 4xx); beklenen 409/429 sayılmaz |
| `checks` | > %99 | Gövde doğrulamaları (kayıt `confirmed`, tarama `ok`, referans = sipariş no …) |
| okuma (`kind:read`) | p95 < 300 ms, p99 < 1 s | Rapor hedefi |
| yazma (`kind:write`) | p95 < 800 ms, p99 < 2 s | Rapor hedefi |
| kimlik (`kind:auth`: kayıt, e-posta onayı, giriş) | p95 < 1,5 s | argon2id bilerek yavaştır ve `PASSWORD_HASH_CONCURRENCY` (4) ile sınırlıdır |
| `checkin` | p95 < 500 ms | Kapıda insan bekler; yazma eşiğinden sıkı |
| `discovery_search` | p95 < 800 ms | En ağır okuma (aday taraması + sıralama + audit) |
| `koro_signup_email_ms` | p95 < 15 s | Doğrulama e-postası: outbox → worker → SMTP (aktarıcı 1 sn'de bir) |

Uç başına p50/p95/p99 tablosu, eşiği olmayan uçlar için de özette yer alır (`max>=0` boş eşikleri bunun içindir).
`stress`'te eşiklerin kalması beklenir: amaç hangi basamakta, hangi uçta bozulduğunu görmektir.

## Sonuç nasıl okunur

- **Job summary:** eşik tablosu (geçti/KALDI), uç tablosu, akış göstergeleri (e-posta gecikmesi, tekrar kayıt, tükenen
  teklif, 429), kalan check'ler, `pg_stat_statements`'tan toplam süreye göre ilk 10 sorgu. İş, k6 bir eşik yüzünden
  çıkarsa (çıkış 99) kırmızı biter; artifact yine yüklenir.
- **Artifact `OPS-005-loadtest-<run>-<deneme>`** (14 gün):
  - `OPS-005-report.html`: k6 web dashboard raporu (zamana göre gecikme/istek/VU grafikleri; tarayıcıda aç),
  - `OPS-005-summary.json` / `.md`: k6 özet verisi ve yukarıdaki Markdown,
  - `OPS-005-k6.json.gz`: ham k6 örnekleri (JSON satırları; `zcat … | jq` ile süz),
  - `OPS-005-api-metrics.txt`, `OPS-005-worker-metrics.txt`: koşu sonundaki Prometheus `/metrics` görüntüsü,
  - `OPS-005-pg-top.txt`: en pahalı 25 sorgu,
  - `OPS-005-docker-stats.log` (15 sn'de bir konteyner CPU/bellek), `OPS-005-resmon.log` (runner belleği/yükü),
    `OPS-005-compose.log.gz` (bütün konteyner logları), `OPS-005-k6.log`.
- Karşılaştırma: aynı profil ve commit'ler arasında göreli bakın (runner donanımı koşudan koşuya biraz değişir).
  p95 artarken CPU'yu dolduran konteyner `docker-stats`'ta, yavaşlayan sorgu `pg-top`'ta görünür.

## Kişisel veri ve sırlar

Bütün veri sentetiktir (`@example.test`, `testutil/fake` adları; yeni hesaplar `yuk.<koşu>.<n>@example.test`). Ortamın
bütün değerleri `.env.example`'ın geliştirme yer tutucularıdır; demo parolası (`KORO_DEMO_PASSWORD`) yalnız bu atılık
ortamı açar. Özet JSON'dan `setup_data` (oturum token'ları) çıkarılır; öğrenci listesi artifact'e konmaz.

## Bakım

- k6 sürümü: workflow'da `K6_VERSION` + `K6_SHA256` (sürüm sayfasındaki `k6-vX.Y.Z-checksums.txt`'den
  `linux-amd64.tar.gz` satırı).
- Yeni bir pilot akışı: `k6/lib/scenarios.js`'e fonksiyon, `config.js`'te `ALL_SCENARIOS`, profil hızları ve
  `ENDPOINTS` (uç adı → `read|write|auth`), `main.js`'te export, workflow'un girdi doğrulamasına ad.
- Yerel deneme gerekirse (önerilmez; Mac yük kuralı): `scripts/serial.sh` kilidiyle, `profile=smoke`, workflow'daki
  adımların aynısı (`docker compose -f loadtest/compose.yaml …`), bitince `docker compose … down -v`.
