GO ?= go
include $(wildcard .env)
export

.PHONY: run build test vet fmt db-up db-down db-reset seed-export

run:
	$(GO) run ./cmd/api

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o bin/api ./cmd/api

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -s -w .

db-up:
	docker compose -f docker-compose.dev.yml up -d --wait

db-down:
	docker compose -f docker-compose.dev.yml down

db-reset:
	docker compose -f docker-compose.dev.yml down -v && $(MAKE) db-up

# Regenerate internal/seed/seed.json from the web app's seed (keeps both sides on the same demo data).
seed-export:
	cd ../sewain-web && TZ=Asia/Jakarta npx --yes tsx@4.20.6 scripts/export-seed.ts ../sewain-api/internal/seed/seed.json
