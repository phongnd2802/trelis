.PHONY: help anydoc-lib build build-lite dev test test-linked vet vet-lite vet-linked lint lint-linked clean

BIN_DIR := bin
BINARY  := $(BIN_DIR)/trelis
PKG     := ./cmd/server

help:
	@echo "Targets:"
	@echo "  anydoc-lib   build the anydoc Rust static archive (needs cargo)"
	@echo "  build        build trelis with the anydoc engine linked (needs anydoc-lib)"
	@echo "  build-lite   build trelis without anydoc (no Rust needed; engine unavailable)"
	@echo "  dev          go run ./cmd/server with anydoc linked (ARGS=... passes flags)"
	@echo "  test         unit tests that need no archive"
	@echo "  test-linked  full tests against the linked anydoc archive"
	@echo "  vet          go vet both build variants (vet-lite / vet-linked for one)"
	@echo "  lint         golangci-lint incl. gofmt, no archive (lint-linked: -tags anydoc)"
	@echo "  clean        remove bin/ and Rust build output"

# The archive is a ~30 MB build artifact and is git-ignored, see
# third_party/anydoc-go/README.md.
anydoc-lib:
	./scripts/build-anydoc-lib.sh

build:
	CGO_ENABLED=1 go build -tags anydoc -o $(BINARY) $(PKG)

build-lite:
	go build -o $(BINARY) $(PKG)

# Runs cmd/server/main.go from the repo root, so relative paths in it resolve
# here. Builds the anydoc archive first if it is missing.
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

# Config in .golangci.yml. Install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
lint:
	golangci-lint run ./...

lint-linked:
	CGO_ENABLED=1 golangci-lint run --build-tags anydoc ./...

clean:
	rm -rf $(BIN_DIR) third_party/anydoc-go/target third_party/anydoc-go/patched-anydoc
