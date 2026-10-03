# koro-backend Makefile. Hedefler `make help` ile listelenir (docs/design/05 §15.3).
# Agent'lar `go test` yerine bu hedefleri kullanır (aynı bayraklar).
SHELL := /bin/bash
.DEFAULT_GOAL := help

GO                    ?= go
GO_TOOLCHAIN          := go1.27.1
GOLANGCI_LINT_VERSION := v2.14.0
GOVULNCHECK_VERSION   := v1.8.0
BIN                   := bin/koro
# Araçlar repo içi bin/ klasörüne kurulur (gitignore'da; global kurulum yok).
# Dosya adı sürümü taşır: sürüm değişince araç kendiliğinden yeniden kurulur.
TOOLS_DIR             := $(CURDIR)/bin
GOLANGCI_LINT         := $(TOOLS_DIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GOVULNCHECK           := $(TOOLS_DIR)/govulncheck-$(GOVULNCHECK_VERSION)
LOAD_ENV              := set -a; [ -f .env ] && . ./.env; set +a;

.PHONY: help tools fmt fmt-check lint test test-integration build sec tidy-check openapi openapi-check \
        migrate-up migrate-down migrate-new deps-up deps-down dev check

help: ## hedefleri listele
	@awk 'BEGIN{FS=":.*## "} /^[a-zA-Z_-]+:.*## /{printf "  %-18s %s\n",$$1,$$2}' $(MAKEFILE_LIST)

# golangci-lint ve govulncheck Go 1.27 ile DERLENMELİ (R-002 §1.5): go install
# minimum toolchain'i (1.26) seçerse ortaya çıkan binary Go 1.27 projesini reddeder.
$(GOLANGCI_LINT):
	@mkdir -p $(TOOLS_DIR)
	GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN=$(TOOLS_DIR) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv $(TOOLS_DIR)/golangci-lint $@
	@$@ --version | grep -q "built with go1.27" || { echo "golangci-lint Go 1.27 ile derlenmemiş"; rm -f $@; exit 1; }

$(GOVULNCHECK):
	@mkdir -p $(TOOLS_DIR)
	GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN=$(TOOLS_DIR) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	@mv $(TOOLS_DIR)/govulncheck $@
	@$(GO) version $@ | grep -q "go1.27" || { echo "govulncheck Go 1.27 ile derlenmemiş"; rm -f $@; exit 1; }

tools: $(GOLANGCI_LINT) $(GOVULNCHECK) ## golangci-lint + govulncheck'i bin/ altına kur (Go 1.27 ile)
	@$(GOLANGCI_LINT) --version

fmt: $(GOLANGCI_LINT) ## gofmt + goimports ile biçimlendir
	$(GOLANGCI_LINT) fmt ./...

fmt-check: $(GOLANGCI_LINT) ## biçim farkı varsa başarısız ol
	@test -z "$$($(GOLANGCI_LINT) fmt --diff ./... 2>&1)" || { echo "biçimlendirme gerekli: make fmt"; exit 1; }

lint: $(GOLANGCI_LINT) ## golangci-lint (config verify + gosec dahil)
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run ./...

test: ## birim + service + yetki matrisi testleri (DB/Redis gerekmez, -race)
	$(GO) test -race -shuffle=on ./...

test-integration: ## testcontainers ile store/queue/migration testleri (Docker gerekir)
	$(GO) test -race -tags=integration -count=1 -p 2 ./...

build: ## tek binary: bin/koro
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -X main.version=$$(git rev-parse --short HEAD 2>/dev/null || echo dev)" -o $(BIN) ./cmd/koro

# Binary modu sembol tablosu ister: -s -w ile soyulmuş binary'de govulncheck
# yalnız modül düzeyinde (çağrılmayan paketler dahil) rapor verir. Aynı kaynaktan
# soyulmamış bir kopya taranır.
sec: $(GOVULNCHECK) $(GOLANGCI_LINT) ## govulncheck (kaynak + binary) ve gosec
	$(GOVULNCHECK) ./...
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN).vulncheck ./cmd/koro
	$(GOVULNCHECK) -mode=binary $(BIN).vulncheck
	$(GOLANGCI_LINT) run --enable-only gosec ./...

tidy-check: ## go.mod/go.sum tidy mi
	$(GO) mod tidy
	@git diff --exit-code -- go.mod go.sum || { echo "go.mod/go.sum tidy değil: go mod tidy && git add go.mod go.sum"; exit 1; }

openapi: ## OpenAPI 3.1 + problem kataloğunu üret (sunucu/DB gerekmez)
	$(GO) run ./cmd/koro openapi -o openapi/openapi.yaml

# Çalışma kopyası indeksle aynı olmalı ve izlenmeyen dosya olmamalı: CI'da
# (temiz checkout) commit'tekiyle aynı demektir; yerelde `git add` sonrası geçer.
openapi-check: openapi ## üretilen dosyalar commit'tekiyle aynı mı
	@git diff --exit-code --stat -- openapi/ && test -z "$$(git ls-files --others --exclude-standard -- openapi/)" \
		|| { echo "openapi/ güncel değil: make openapi && git add openapi/"; git status --short -- openapi/; exit 1; }

migrate-up: ## bekleyen migration'ları uygula (MIGRATE_DATABASE_URL, owner rolü)
	$(LOAD_ENV) $(GO) run ./cmd/koro migrate up

migrate-down: ## n adım geri al: make migrate-down n=1 (n ZORUNLU)
	@test -n "$(n)" || { echo "kullanım: make migrate-down n=<adım>"; exit 1; }
	$(LOAD_ENV) $(GO) run ./cmd/koro migrate down $(n)

migrate-new: ## sıradaki migration çiftini aç: make migrate-new name=create_events
	@test -n "$(name)" || { echo "kullanım: make migrate-new name=<snake_case>"; exit 1; }
	@echo "$(name)" | grep -Eq "^[a-z][a-z0-9_]*$$" || { echo "name snake_case olmalı"; exit 1; }
	@mkdir -p db/migrations
	@last=$$(ls db/migrations/*.up.sql 2>/dev/null | sed -E 's|.*/0*([0-9]+)_.*|\1|' | sort -n | tail -1); \
	next=$$(printf "%06d" $$(( $${last:-0} + 1 ))); \
	printf -- "-- $${next}_$(name).up.sql\nBEGIN;\nSET LOCAL lock_timeout = '5s';\n\nCOMMIT;\n" > db/migrations/$${next}_$(name).up.sql; \
	printf -- "-- $${next}_$(name).down.sql\nBEGIN;\n\nCOMMIT;\n" > db/migrations/$${next}_$(name).down.sql; \
	echo "oluşturuldu: db/migrations/$${next}_$(name).{up,down}.sql  (CONCURRENTLY ise BEGIN/COMMIT'i sil, tek ifade bırak)"

deps-up: ## yerel postgres + redis + mailpit + seaweedfs (compose: BE-002)
	docker compose -f deploy/compose.dev.yaml up -d --wait
	docker compose -f deploy/compose.dev.yaml wait seaweedfs-buckets >/dev/null

deps-down: ## yerel bağımlılıkları durdur
	docker compose -f deploy/compose.dev.yaml down

dev: deps-up migrate-up ## bağımlılıklar + migration + api + worker (Ctrl-C ile hepsi durur)
	@$(LOAD_ENV) trap 'kill 0' INT TERM; \
	$(GO) run ./cmd/koro worker & \
	$(GO) run ./cmd/koro api & \
	wait

check: fmt-check lint test openapi-check tidy-check sec ## PR öncesi tam kontrol (test-integration ayrı)
