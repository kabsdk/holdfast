# Holdfast developer commands. Every target runs on Linux (WSL2 is fine).
.DEFAULT_GOAL := build

PG_IMAGE    ?= postgres:17-alpine
PG_PORT     ?= 55432
PG_NAME     ?= holdfast-test-pg
export HOLDFAST_TEST_PG ?= postgres://postgres:holdfast@127.0.0.1:$(PG_PORT)/postgres?sslmode=disable

# MinIO for the e2e tests. MinIO's community images left Docker Hub; Chainguard rebuilds them from
# source. Pinned by digest (plan-m1 Q15).
MINIO_IMAGE ?= cgr.dev/chainguard/minio@sha256:e9cf6b99c226e9539ed0f14b6f00e37b07da153c7ba1f4f8709c807574b471ec
MC_IMAGE    ?= cgr.dev/chainguard/minio-client@sha256:94433f625c8057d18c69675dbaa33da6e4864f084f9c44faf30d3eb8e3754e0b
MINIO_NAME  ?= holdfast-test-minio
MINIO_PORT  ?= 59000
S3_USER     ?= hfroot
S3_PASS     ?= hfroot-e2e-secret
GO_IMAGE    ?= golang:1.26
TOOLS       ?= $(HOME)/.cache/holdfast
RESTIC_VERSION ?= 0.19.1

PROM_IMAGE  ?= prom/prometheus:v2.53.0

.PHONY: build test unit vet fmt pg-up pg-down minio-up minio-down tools e2e rules-test

# promtool: the alert rules parse, and their unit tests pass.
rules-test:
	docker run --rm -v $(CURDIR)/deploy/prometheus:/rules:ro -w /rules --entrypoint promtool $(PROM_IMAGE) check rules holdfast-alerts.yml
	docker run --rm -v $(CURDIR)/deploy/prometheus:/rules:ro -w /rules --entrypoint promtool $(PROM_IMAGE) test rules holdfast-alerts.test.yml

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

minio-up:
	@docker inspect $(MINIO_NAME) >/dev/null 2>&1 || docker run -d --name $(MINIO_NAME) -p 127.0.0.1:$(MINIO_PORT):9000 \
		-e MINIO_ROOT_USER=$(S3_USER) -e MINIO_ROOT_PASSWORD=$(S3_PASS) $(MINIO_IMAGE) server /data >/dev/null
	@for i in $$(seq 1 30); do MC_HOST_hf=http://$(S3_USER):$(S3_PASS)@127.0.0.1:$(MINIO_PORT) $(TOOLS)/mc ready hf >/dev/null 2>&1 && break; sleep 1; done

minio-down:
	-docker rm -f $(MINIO_NAME)

# restic (checksum-verified) and mc (taken from the pinned client image) for the e2e tests.
tools:
	@scripts/fetch-restic.sh $(RESTIC_VERSION) >/dev/null
	@test -x $(TOOLS)/mc || { id=$$(docker create $(MC_IMAGE)); docker cp $$id:/usr/bin/mc $(TOOLS)/mc; docker rm $$id >/dev/null; }

# The end-to-end suite, as root in a privileged container (the metadata tests need root), on an ext4
# volume so ACLs, trusted.* xattrs and SELinux labels behave as on a host. --init gives the container
# a PID 1 that reaps orphans, as systemd does on a host: without it a SIGKILLed restic stays a zombie,
# and restic rightly will not call a lock stale while its process still exists.
E2E_ARGS ?=
e2e: tools pg-up minio-up
	docker run --rm --init --privileged --network host -v $(CURDIR):/src -w /src \
		-v holdfast-gomod:/go/pkg/mod -v holdfast-gocache:/root/.cache/go-build -v holdfast-e2e-work:/work \
		-v $(TOOLS):/tools:ro -e TMPDIR=/work -e GOFLAGS=-buildvcs=false \
		-e "HOLDFAST_TEST_PG=$(HOLDFAST_TEST_PG)" -e HOLDFAST_S3_ENDPOINT=http://127.0.0.1:$(MINIO_PORT) \
		-e HOLDFAST_S3_ROOT_USER=$(S3_USER) -e HOLDFAST_S3_ROOT_PASSWORD=$(S3_PASS) \
		-e HOLDFAST_RESTIC=/tools/restic-$(RESTIC_VERSION) -e HOLDFAST_MC=/tools/mc \
		$(GO_IMAGE) go test ./test/e2e/ -count=1 -v -timeout 40m $(E2E_ARGS)
