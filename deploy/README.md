# Dağıtım (Dokploy)

Tek imaj, üç servis (06 H-8, 05 §1.1). İmaj **CI'da** üretilir (`deploy/Dockerfile`), etiketi git SHA'dır (`latest` yok);
aynı imaj staging'den prod'a terfi eder. İmajda secret yoktur; secret'lar Dokploy secret mount'u ile `*_FILE` olarak verilir
(ör. `PASSWORD_PEPPER_FILE=/run/secrets/password_pepper`). Değişkenlerin listesi `.env.example`'dadır.

| Servis | Komut | Not |
|---|---|---|
| `koro-migrate` | `migrate up` | Tek seferlik iş; `koro-api`/`koro-worker` yeni sürüme geçmeden **önce** çalışır. Owner rolü `app_owner`: `MIGRATE_DATABASE_URL`, **doğrudan PostgreSQL'e** (PgBouncer değil; advisory lock). Docker HEALTHCHECK'i kapatın (iş HTTP sunmaz). `dirty` kalırsa: hatayı düzelt → `migrate force <sürüm>` → `migrate up`. |
| `koro-api` | `api` | Port 8080 (`HTTP_ADDR`), Traefik arkasında. HEALTHCHECK imajda tanımlı (`koro healthcheck` → `/healthz`). Hazırlık: `/readyz` (Traefik sağlık kontrolü buraya bakar). |
| `koro-worker` | `worker` | asynq + scheduler + outbox aktarıcısı (BE-006). HTTP sunmadığı için imajdaki HEALTHCHECK'i kapatın ya da BE-006'nın tanımlayacağı kontrolle değiştirin. |

Dokploy/Swarm ayarları:
- `stop_grace_period` **≥ 40s**: kapanış sırası sinyal → `/readyz` 503 → `SHUTDOWN_DELAY` (5 sn) → `SHUTDOWN_TIMEOUT` (25 sn) + pay (05 §10.9). Docker'ın varsayılan 10 sn'si uçuştaki istekleri keser.
- `EXPOSE_OPENAPI=false` (prod'da zorunlu; doğrulama açılışta reddeder). `/metrics` yayımlanmaz (iç port, ileride).
- `APP_ENV=staging|prod` iken `TRUSTED_ORIGINS` ve `PUBLIC_BASE_URL` `https://` olmalı; `LOG_FORMAT=json`.
- Traefik idle timeout, sunucunun `IdleTimeout` değerinden (90 sn) küçük olmalı.

Yerel deneme:

```bash
docker build -f deploy/Dockerfile --build-arg GIT_SHA=$(git rev-parse --short HEAD) -t koro:$(git rev-parse --short HEAD) .
docker run --rm -p 8080:8080 \
  -e APP_ENV=dev -e PUBLIC_BASE_URL=http://localhost:3000 -e TRUSTED_ORIGINS=http://localhost:3000 \
  koro:$(git rev-parse --short HEAD) api
curl -i localhost:8080/healthz
```

## Veritabanı (BE-002)

- **Kurulum (bir kez, OPS):** veritabanı ICU `tr-TR` ile açılır (`initdb --encoding=UTF8 --locale-provider=icu
  --icu-locale=tr-TR --locale=C.UTF-8`); süper kullanıcıyla `psql -d koro -f deploy/db/bootstrap.sql` rolleri
  (`app_owner`, `app_rw`, `app_ro`, `audit_writer`, `audit_chainer`, `app_talent`) ve DB sahipliğini kurar; parolalar
  secret'tan `ALTER ROLE … PASSWORD` ile verilir. `deploy/db/dev-passwords.sql` **yalnız** yerel/testtir.
- `app_owner` süper kullanıcı değildir; migration'lar yalnız `GRANT/REVOKE` yapar, rol oluşturmaz.
- `koro-api` / `koro-worker`: `DATABASE_URL` (`app_rw`). `/readyz`: DB ping + şema sürümü ≥ imajdaki en yeni migration
  ve `dirty` değil (yuvarlanan dağıtımda migrate işi önce koştuğu için eski sürüm hazır kalır).
- **ICU sürümü değişince** (PostgreSQL imajı/OS güncellemesi; T-41): `SELECT collname, collversion,
  pg_collation_actual_version(oid) FROM pg_collation WHERE collname IN ('tr','tr_ci');` farklıysa `koro_norm(...)` /
  `COLLATE "tr"` kullanan indeksler `REINDEX` edilir, sonra `ALTER COLLATION tr REFRESH VERSION` (ve `tr_ci`) ve
  `ALTER DATABASE koro REFRESH COLLATION VERSION`.

## Yerel geliştirme

```bash
cp .env.example .env      # dev yer tutucuları; gerçek secret yok
make deps-up              # postgres 18 (ICU tr-TR) + redis 8 + mailpit + seaweedfs; bucket'lar hazır olunca döner
make migrate-up           # app_owner ile 000001…
make dev                  # deps-up + migrate-up + api + worker
make deps-down            # durdur (veri volume'larda kalır; sıfırlamak: docker compose -f deploy/compose.dev.yaml down -v)
```

| Servis | Adres | Not |
|---|---|---|
| PostgreSQL | `127.0.0.1:5432`, db `koro` | `app_rw` / `app_owner` parolası `dev-only-change-me` |
| Redis | `127.0.0.1:6379` | AOF, `noeviction` |
| Mailpit | SMTP `127.0.0.1:1025`, arayüz `http://127.0.0.1:8025` | |
| SeaweedFS S3 | `http://127.0.0.1:8333` | anahtar `koro-dev-access-key` / `dev-only-change-me` (`deploy/seaweedfs/s3.dev.json`); bucket'lar `koro-files-dev`, `koro-quarantine-dev`, anonim erişim yok |
