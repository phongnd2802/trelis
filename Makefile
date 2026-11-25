.PHONY: help tools sqlc anydoc-lib build build-lite dev test test-linked vet vet-lite vet-linked lint lint-linked stack-up clean

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
	@echo "  vet          go vet both build variants (vet-lite / vet-linked for one)"
	@echo "  lint         golangci-lint incl. gofmt, no archive (lint-linked: -tags anydoc)"
	@echo "  stack-up     docker compose up --build and wait; on failure print the broken services' logs"
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

vet: vet-lite vet-linked

vet-lite:
	go vet ./...

vet-linked:
	CGO_ENABLED=1 go vet -tags anydoc ./...

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
	$(GOLANGCI_LINT) run ./...

lint-linked:
	CGO_ENABLED=1 $(GOLANGCI_LINT) run --build-tags anydoc ./...

stack-up:
	./scripts/stack-up.sh

clean:
	rm -rf $(BIN_DIR) third_party/anydoc-go/target third_party/anydoc-go/patched-anydoc
