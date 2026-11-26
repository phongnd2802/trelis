# Trelis

Trelis answers questions over a document collection and shows, with numbers,
which retrieval techniques actually help. The domain language is in
[GLOSSARY.md](GLOSSARY.md) and the design decisions in [docs/adr/](docs/adr/).

## Running it

```sh
make dev-up   # Postgres, Neo4j and Temporal in Docker (compose.dev.yaml)
make dev      # Trelis on the host, with the anydoc engine linked
```

`make stack-up` runs the whole stack, Trelis included, in Docker instead.
Run `make help` for every target.

## Tests

```sh
make test              # unit tests; no Docker, no Rust
make test-integration  # the HTTP API end to end; needs Docker
```

The integration tests in [internal/integration](internal/integration) build
Trelis, start Postgres (ParadeDB) and Neo4j with
[testcontainers](https://golang.testcontainers.org/), and drive each Trelis
process only through its HTTP API. Models are never called: every test gets
a fake OpenAI-compatible provider (`modeltest.FakeProvider` in
[internal/models/modeltest](internal/models/modeltest))
that serves Chat Completions, Responses, embeddings and rerank and answers
from what the test scripted. The first run pulls the container images, so it
takes a few minutes; later runs take about half a minute.

They need a Docker daemon the current user can reach. With Podman or a
non-default socket, set `DOCKER_HOST` (and, for rootless Podman,
`TESTCONTAINERS_RYUK_DISABLED=true`). To run one test:

```sh
go test -tags integration -count=1 -run TestMe ./internal/integration/
```
