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
RCLONE_VERSION ?= 1.76.0

PROM_IMAGE  ?= prom/prometheus:v2.53.0

# Packages (plan-m5 item 1): nfpm in a container, pinned by digest. HOLDFAST_VERSION is a release's version,
# such as 0.5.0; a build between releases sorts before the next one (scripts/version.sh). Worked out once.
NFPM_IMAGE ?= goreleaser/nfpm@sha256:f1e9f1adcf452a85ab7765aa7252cdb2f94816ead5363bbd52a4e563087c942b
ifeq ($(origin HOLDFAST_VERSION),undefined)
HOLDFAST_VERSION := $(shell sh scripts/version.sh)
endif
# Static binaries, so one package runs on every distribution; the agent reports its version.
export CGO_ENABLED ?= 0
LDFLAGS := -X github.com/kabsdk/holdfast/internal/agent.Version=$(HOLDFAST_VERSION) -X github.com/kabsdk/holdfast/internal/server.Version=$(HOLDFAST_VERSION)

# The login and UI tests (M4): a real OIDC provider, and a headless browser driven over the DevTools
# protocol. Both pinned by digest; both on the host network, on loopback only.
AUTHELIA_IMAGE ?= authelia/authelia@sha256:bd97cff4fcbf715b5ff1f9ae286afbe6033afce385302520b0368122d43a6f54
BROWSER_IMAGE  ?= chromedp/headless-shell@sha256:381353d3530ab9d579d2fe8b9c0f4519c995617f5a71daf6bb649e2d12005bd9
BROWSER_NAME   ?= holdfast-test-browser
BROWSER_PORT   ?= 59222

.PHONY: build test unit vet fmt pg-up pg-down minio-up minio-down tools e2e e2e-image rules-test authelia-up authelia-down browser-up browser-down packages

# promtool: the alert rules parse, and their unit tests pass.
rules-test:
	docker run --rm -v $(CURDIR)/deploy/prometheus:/rules:ro -w /rules --entrypoint promtool $(PROM_IMAGE) check rules holdfast-alerts.yml
	docker run --rm -v $(CURDIR)/deploy/prometheus:/rules:ro -w /rules --entrypoint promtool $(PROM_IMAGE) test rules holdfast-alerts.test.yml

build:
	go build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

# The .deb and .rpm packages, with SHA256SUMS, in dist/ (packaging/build.sh). Unsigned (plan-m5 Q2).
packages: build
	@HOLDFAST_VERSION='$(HOLDFAST_VERSION)' RESTIC_VERSION=$(RESTIC_VERSION) NFPM_IMAGE=$(NFPM_IMAGE) \
		RESTIC_BINARY=$$(scripts/fetch-restic.sh $(RESTIC_VERSION)) RCLONE_BINARY=$$(scripts/fetch-rclone.sh $(RCLONE_VERSION)) bash packaging/build.sh

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

# Authelia for the login tests: its state (a test CA, keys, users) is made once in $(TOOLS)/authelia-e2e.
authelia-up:
	@bash test/e2e/authelia/setup.sh $(TOOLS)/authelia-e2e $(AUTHELIA_IMAGE)

authelia-down:
	-docker rm -f holdfast-test-authelia

# Chrome for the UI tests. *.holdfast.test resolve to loopback; the test CA is not installed in it.
browser-up:
	@docker inspect $(BROWSER_NAME) >/dev/null 2>&1 || docker run -d --name $(BROWSER_NAME) --network host --entrypoint /headless-shell/headless-shell \
		$(BROWSER_IMAGE) --no-sandbox --remote-debugging-address=127.0.0.1 --remote-debugging-port=$(BROWSER_PORT) \
		'--host-resolver-rules=MAP *.holdfast.test 127.0.0.1' --ignore-certificate-errors >/dev/null
	@for i in $$(seq 1 30); do curl -fs http://127.0.0.1:$(BROWSER_PORT)/json/version >/dev/null 2>&1 && break; sleep 1; done

browser-down:
	-docker rm -f $(BROWSER_NAME)

# restic and rclone (checksum-verified) and mc (taken from the pinned client image) for the e2e tests.
tools:
	@scripts/fetch-restic.sh $(RESTIC_VERSION) >/dev/null
	@scripts/fetch-rclone.sh $(RCLONE_VERSION) >/dev/null
	@test -x $(TOOLS)/mc || { id=$$(docker create $(MC_IMAGE)); docker cp $$id:/usr/bin/mc $(TOOLS)/mc; docker rm $$id >/dev/null; }

# The end-to-end suite, as root in a privileged container (the metadata tests need root), on an ext4
# volume so ACLs, trusted.* xattrs and SELinux labels behave as on a host. --init gives the container
# a PID 1 that reaps orphans, as systemd does on a host: without it a SIGKILLed restic stays a zombie,
# and restic rightly will not call a lock stale while its process still exists.
E2E_ARGS ?=
E2E_IMAGE ?= holdfast-e2e:local
e2e-image:
	@docker build -q -t $(E2E_IMAGE) --build-arg GO_IMAGE=$(GO_IMAGE) test/e2e >/dev/null
e2e: tools pg-up minio-up authelia-up browser-up e2e-image
	docker run --rm --init --privileged --network host -v $(CURDIR):/src -w /src \
		-v holdfast-gomod:/go/pkg/mod -v holdfast-gocache:/root/.cache/go-build -v holdfast-e2e-work:/work \
		-v $(TOOLS):/tools:ro -e TMPDIR=/work -e GOFLAGS=-buildvcs=false \
		-e "HOLDFAST_TEST_PG=$(HOLDFAST_TEST_PG)" -e HOLDFAST_S3_ENDPOINT=http://127.0.0.1:$(MINIO_PORT) \
		-e HOLDFAST_S3_ROOT_USER=$(S3_USER) -e HOLDFAST_S3_ROOT_PASSWORD=$(S3_PASS) \
		-e HOLDFAST_RESTIC=/tools/restic-$(RESTIC_VERSION) -e HOLDFAST_MC=/tools/mc -e HOLDFAST_RCLONE=/tools/rclone-$(RCLONE_VERSION) \
		-e HOLDFAST_OIDC_DIR=/tools/authelia-e2e -e HOLDFAST_BROWSER=http://127.0.0.1:$(BROWSER_PORT) \
		$(E2E_IMAGE) go test ./test/e2e/ -count=1 -v -timeout 60m $(E2E_ARGS)
