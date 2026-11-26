# syntax=docker/dockerfile:1.7
# Trelis with the anydoc engine linked. Three stages: Rust builds the anydoc
# static archive, Go links it with cgo, distroless/cc runs the binary.

FROM rust:1.100.0-bookworm AS anydoc
RUN apt-get update && apt-get install -y --no-install-recommends binutils && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY scripts/build-anydoc-lib.sh scripts/
COPY third_party/anydoc-go third_party/anydoc-go
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/third_party/anydoc-go/target \
    scripts/build-anydoc-lib.sh

FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/anydoc-go/go.mod third_party/anydoc-go/
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=anydoc /src/third_party/anydoc-go/lib third_party/anydoc-go/lib
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -tags anydoc -trimpath -o /out/trelis ./cmd/server
# distroless has no shell, so the upload directory is made here.
RUN mkdir -p /out/data/files

FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=build /out/trelis /trelis
COPY --from=build --chown=65532:65532 /out/data /data
COPY config/trelis.yaml /etc/trelis/trelis.yaml
EXPOSE 8080
ENTRYPOINT ["/trelis", "-config", "/etc/trelis/trelis.yaml"]
