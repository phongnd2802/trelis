.PHONY: help anydoc-lib build build-lite test test-linked vet clean

BIN_DIR := bin
BINARY  := $(BIN_DIR)/trelis
PKG     := ./cmd/trelis

help:
	@echo "Targets:"
	@echo "  anydoc-lib   build the anydoc Rust static archive (needs cargo)"
	@echo "  build        build trelis with the anydoc engine linked (needs anydoc-lib)"
	@echo "  build-lite   build trelis without anydoc (no Rust needed; engine unavailable)"
	@echo "  test         unit tests that need no archive"
	@echo "  test-linked  full tests against the linked anydoc archive"
	@echo "  vet          go vet both build variants"
	@echo "  clean        remove bin/ and Rust build output"

# The archive is a ~30 MB build artifact and is git-ignored, see
# third_party/anydoc-go/README.md.
anydoc-lib:
	./scripts/build-anydoc-lib.sh

build:
	CGO_ENABLED=1 go build -tags anydoc -o $(BINARY) $(PKG)

build-lite:
	go build -o $(BINARY) $(PKG)

test:
	go test ./...

test-linked:
	CGO_ENABLED=1 go test -tags anydoc ./...

vet:
	go vet ./...
	CGO_ENABLED=1 go vet -tags anydoc ./...

clean:
	rm -rf $(BIN_DIR) third_party/anydoc-go/target third_party/anydoc-go/patched-anydoc
