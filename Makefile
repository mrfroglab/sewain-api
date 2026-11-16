# Go lokal kalau ada, kalau tidak lewat Docker (scripts/go.sh).
GO ?= $(if $(shell command -v go 2>/dev/null),go,scripts/go.sh)
include $(wildcard .env)
export

.PHONY: run build test test-integration vet fmt db-up db-down db-reset seed-export

run:
	$(GO) run ./cmd/api

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o bin/api ./cmd/api

test:
	$(GO) test ./...

# Tes + tes integrasi dengan Postgres/PostGIS sekali pakai (satu container per worktree, lihat scripts/test-db.sh).
test-integration:
	TEST_DATABASE_URL=$$(scripts/test-db.sh up) $(GO) test -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	$(if $(shell command -v gofmt 2>/dev/null),gofmt -s -w .,$(GO) fmt ./...)

db-up:
	docker compose -f docker-compose.dev.yml up -d --wait

db-down:
	docker compose -f docker-compose.dev.yml down

db-reset:
	docker compose -f docker-compose.dev.yml down -v && $(MAKE) db-up

# Regenerate internal/seed/seed.json from the web app's seed (keeps both sides on the same demo data).
seed-export:
	cd ../sewain-web && TZ=Asia/Jakarta npx --yes tsx@4.20.6 scripts/export-seed.ts ../sewain-api/internal/seed/seed.json
