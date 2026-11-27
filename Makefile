.PHONY: help tools sqlc swagger anydoc-lib build build-lite dev test test-linked test-integration test-integration-linked vet vet-lite vet-linked lint lint-linked eval-lint eval-test stack-up dev-up dev-down clean

BIN_DIR := bin
BINARY  := $(BIN_DIR)/trelis
PKG     := ./cmd/server

help:
	@echo "Targets:"
	@echo "  tools        install pinned dev tools (golangci-lint) into GOPATH/bin"
	@echo "  sqlc         regenerate internal/db/sqlc from migrations/ and internal/db/queries/"
	@echo "  swagger      regenerate internal/apidocs from the handlers' swag annotations"
	@echo "  anydoc-lib   build the anydoc Rust static archive (needs cargo)"
	@echo "  build        build trelis with the anydoc engine linked (needs anydoc-lib)"
	@echo "  build-lite   build trelis without anydoc (no Rust needed; engine unavailable)"
	@echo "  dev          go run ./cmd/server with anydoc linked (ARGS=... passes flags)"
	@echo "  test         unit tests; the Postgres tests too when the dev database is up"
	@echo "  test-linked  full tests against the linked anydoc archive"
	@echo "  eval-lint    ruff lint and format check of the eval harness (needs uv)"
	@echo "  eval-test    eval harness tests (cd eval && uv run pytest; needs uv)"
	@echo "  test-integration  HTTP API tests against Postgres, Neo4j, silo (S3) and Temporal in testcontainers (needs Docker)"
	@echo "  test-integration-linked  the same with the anydoc engine linked (needs anydoc-lib)"
	@echo "  vet          go vet both build variants (vet-lite / vet-linked for one)"
	@echo "  lint         golangci-lint incl. gofmt, no archive (lint-linked: -tags anydoc); NOTICE and feature-map checks"
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
# Exports .local/models.env (gitignored) when it exists, so model roles and
# their keys (TRELIS_MODELS_*) stay out of the tracked config file.
ANYDOC_ARCHIVE := third_party/anydoc-go/lib/linux_amd64_gnu/libanydoc_go.a

dev:
	@test -f $(ANYDOC_ARCHIVE) || $(MAKE) anydoc-lib
	@if [ -f .local/models.env ]; then set -a; . ./.local/models.env; set +a; fi; \
	CGO_ENABLED=1 go run -tags anydoc ./cmd/server $(ARGS)

# The Postgres-backed tests skip themselves when TRELIS_TEST_POSTGRES_DSN is
# unset. The script prints an explicit DSN, else the dev Postgres
# (compose.dev.yaml) when it is up, and the skip hint when it finds no server.
TEST_DB_ENV = export TRELIS_TEST_POSTGRES_DSN="$$(scripts/test-db-env.sh)";

test:
	@$(TEST_DB_ENV) go test ./...

test-linked:
	@$(TEST_DB_ENV) CGO_ENABLED=1 go test -tags anydoc ./...

# The external Python eval harness (eval/, ADR-0001); no Go build needed.
# Kept out of lint/test so the Go jobs need no uv; CI runs both in its eval job.
eval-lint:
	cd eval && uv run ruff check . && uv run ruff format --check .

eval-test:
	cd eval && uv run pytest

# Behind the integration build tag so `make test` needs no Docker; see
# internal/integration. -count=1 because the cache cannot see the containers.
test-integration:
	go test -tags integration -count=1 ./internal/integration/...

# The same tests with the anydoc engine linked, so the docx happy path runs.
test-integration-linked:
	CGO_ENABLED=1 go test -tags integration,anydoc -count=1 ./internal/integration/...

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
SWAG_VERSION  := v1.16.6

tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Config in sqlc.yaml. The generated code is committed, so building needs no
# sqlc; `go run` with a pinned version keeps it out of `make tools` and CI.
sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

# The generated spec is committed, so building needs no swag; `go run` with a
# pinned version keeps it out of `make tools` and CI. The general API Info
# lives in internal/router, so -g is read relative to that first -d directory.
swagger:
	go run github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION) init -g router.go -d ./internal/router,./internal/handler,./internal/types,./internal/errors -o internal/apidocs --outputTypes go,json,yaml --packageName apidocs

lint:
	$(GOLANGCI_LINT) run --build-tags integration ./...
	scripts/check-notice.sh
	scripts/check-feature-map.sh

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
