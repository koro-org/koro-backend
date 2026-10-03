# Dağıtım (Dokploy)

Tek imaj, üç servis (06 H-8, 05 §1.1). İmaj **CI'da** üretilir (`deploy/Dockerfile`), etiketi git SHA'dır (`latest` yok);
aynı imaj staging'den prod'a terfi eder. İmajda secret yoktur; secret'lar Dokploy secret mount'u ile `*_FILE` olarak verilir
(ör. `PASSWORD_PEPPER_FILE=/run/secrets/password_pepper`). Değişkenlerin listesi `.env.example`'dadır.

| Servis | Komut | Not |
|---|---|---|
| `koro-migrate` | `migrate up` | Tek seferlik iş; `koro-api`/`koro-worker` yeni sürüme geçmeden **önce** çalışır. Owner rolü: `MIGRATE_DATABASE_URL` (BE-002). Docker HEALTHCHECK'i kapatın (iş HTTP sunmaz). |
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
