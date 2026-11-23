# The e2e suite's image: Go, plus PostgreSQL's client tools for the dump test. make e2e builds it
# once; Docker's cache makes later builds instant.
ARG GO_IMAGE=golang:1.26
FROM ${GO_IMAGE}
RUN apt-get update && apt-get install -y --no-install-recommends postgresql-client && rm -rf /var/lib/apt/lists/*
