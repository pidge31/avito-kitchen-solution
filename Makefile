GO ?= go

.PHONY: up down logs build fmt lint test test-integration contract smoke check check-db require-test-db

up:
	docker compose up --build -d --wait

down:
	docker compose down

logs:
	docker compose logs -f api restaurant

build:
	$(GO) build -o bin/api ./cmd/api
	$(GO) build -o bin/restaurant ./cmd/restaurant

fmt:
	gofmt -w cmd internal migrations openapi

lint:
	@files="$$(gofmt -l cmd internal migrations openapi)"; if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi
	$(GO) vet -all ./...

test:
	$(GO) test -race -count=1 ./...

test-integration:
	@test -n "$(TEST_DATABASE_URL)" || (echo 'TEST_DATABASE_URL is required (PostgreSQL URL)'; exit 1)
	$(GO) test -race -count=1 -v ./internal/httpapi -run Integration

contract:
	$(GO) run ./cmd/check-openapi

smoke:
	$(GO) run ./cmd/smoke

check:
	@bash scripts/check.sh "$(GO)"

require-test-db:
	@test -n "$(TEST_DATABASE_URL)" || (echo 'TEST_DATABASE_URL is required; use make check to create a test database'; exit 1)

check-db: require-test-db lint test contract
