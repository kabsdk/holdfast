# Holdfast developer commands. Every target runs on Linux (WSL2 is fine).

PG_IMAGE    ?= postgres:17-alpine
PG_PORT     ?= 55432
PG_NAME     ?= holdfast-test-pg
export HOLDFAST_TEST_PG ?= postgres://postgres:holdfast@127.0.0.1:$(PG_PORT)/postgres?sslmode=disable

.PHONY: build test unit vet fmt pg-up pg-down

build:
	go build -o bin/ ./cmd/...

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

# Unit tests, plus the store tests when the test PostgreSQL is up (make pg-up).
unit:
	go test ./... -count=1

test: vet unit

# A throwaway PostgreSQL for the store and e2e tests.
pg-up:
	@docker inspect $(PG_NAME) >/dev/null 2>&1 || docker run -d --name $(PG_NAME) -e POSTGRES_PASSWORD=holdfast -p 127.0.0.1:$(PG_PORT):5432 $(PG_IMAGE) >/dev/null
	@for i in $$(seq 1 30); do docker exec $(PG_NAME) pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
	@echo "HOLDFAST_TEST_PG=$(HOLDFAST_TEST_PG)"

pg-down:
	-docker rm -f $(PG_NAME)
