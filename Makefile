.PHONY: help tools sqlc anydoc-lib build build-lite dev test test-linked test-integration vet vet-lite vet-linked lint lint-linked stack-up dev-up dev-down clean

BIN_DIR := bin
BINARY  := $(BIN_DIR)/trelis
PKG     := ./cmd/server

help:
	@echo "Targets:"
	@echo "  tools        install pinned dev tools (golangci-lint) into GOPATH/bin"
	@echo "  sqlc         regenerate internal/db/sqlc from migrations/ and internal/db/queries/"
	@echo "  anydoc-lib   build the anydoc Rust static archive (needs cargo)"
	@echo "  build        build trelis with the anydoc engine linked (needs anydoc-lib)"
	@echo "  build-lite   build trelis without anydoc (no Rust needed; engine unavailable)"
	@echo "  dev          go run ./cmd/server with anydoc linked (ARGS=... passes flags)"
	@echo "  test         unit tests that need no archive"
	@echo "  test-linked  full tests against the linked anydoc archive"
	@echo "  test-integration  HTTP API tests against Postgres, Neo4j and silo (S3) in testcontainers (needs Docker)"
	@echo "  vet          go vet both build variants (vet-lite / vet-linked for one)"
	@echo "  lint         golangci-lint incl. gofmt, no archive (lint-linked: -tags anydoc); NOTICE check"
	@echo "  dev-up       start only the dependencies (compose.dev.yaml) for \`make dev\`, and wait"
	@echo "  dev-down     stop the local stack (dev or full), keeping its volumes"
	@echo "  stack-up     full stack (compose.yaml, builds trelis) and wait; on failure print the broken services' logs"
	@echo "  clean        remove bin/ and Rust build output"

# The archive is a ~30 MB build artifact and is git-ignored, see
# third_party/anydoc-go/README.md.
anydoc-lib:
	./scripts/build-anydoc-lib.sh

build:
	CGO_ENABLED=1 go build -tags anydoc -o $(BINARY) $(PKG)

build-lite:
	go build -o $(BINARY) $(PKG)

# Runs the service from the repo root, so the default -config path
# (config/trelis.yaml) resolves. Builds the anydoc archive first if missing.
ANYDOC_ARCHIVE := third_party/anydoc-go/lib/linux_amd64_gnu/libanydoc_go.a

dev:
	@test -f $(ANYDOC_ARCHIVE) || $(MAKE) anydoc-lib
	CGO_ENABLED=1 go run -tags anydoc ./cmd/server $(ARGS)

test:
	go test ./...

test-linked:
	CGO_ENABLED=1 go test -tags anydoc ./...

# Behind the integration build tag so `make test` needs no Docker; see
# internal/integration. -count=1 because the cache cannot see the containers.
test-integration:
	go test -tags integration -count=1 ./internal/integration/...

vet: vet-lite vet-linked

# The integration tag only adds test files, so vetting and linting with it
# also covers the plain build.
vet-lite:
	go vet -tags integration ./...

vet-linked:
	CGO_ENABLED=1 go vet -tags anydoc,integration ./...

# Config in .golangci.yml. Resolved from GOPATH/bin so it works without
# GOPATH/bin on PATH; `make tools` installs the pinned version there.
GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT ?= $(shell go env GOPATH)/bin/golangci-lint
SQLC_VERSION  := v1.30.0

tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Config in sqlc.yaml. The generated code is committed, so building needs no
# sqlc; `go run` with a pinned version keeps it out of `make tools` and CI.
sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

lint:
	$(GOLANGCI_LINT) run --build-tags integration ./...
	scripts/check-notice.sh

lint-linked:
	CGO_ENABLED=1 $(GOLANGCI_LINT) run --build-tags anydoc,integration ./...

stack-up:
	./scripts/stack-up.sh

# The dependencies `make dev` needs, without building the trelis image.
# docker compose reads COMPOSE_FILE, so stack-up.sh needs no flag for it.
dev-up:
	COMPOSE_FILE=compose.dev.yaml ./scripts/stack-up.sh

# Through compose.yaml, which includes every service, so it also stops a
# trelis container left by `make stack-up` instead of orphaning it.
dev-down:
	docker compose down

clean:
	rm -rf $(BIN_DIR) third_party/anydoc-go/target third_party/anydoc-go/patched-anydoc
