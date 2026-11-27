# Trelis eval harness

A separate Python program that measures Trelis from the outside
([ADR-0001](../docs/adr/0001-external-python-eval-harness.md)). It talks to
a running Trelis through the public HTTP API as one Principal, with that
Principal's API key, so every run exercises the same path a real client takes.
It never imports Go code, reads Postgres or calls the admin CLI; results are
versioned files, not server state.

The current skeleton fetches the MultiHop-RAG Benchmark Corpus, ingests a
subset as Documents, waits for ingest to finish, and reads and writes Golden
Sets. Retrieval metrics, the record/replay proxy and stratified sampling come
in later tickets.

Terms are the ones in [GLOSSARY.md](../GLOSSARY.md): Corpus, Benchmark
Corpus, Document, Principal, Access List, Golden Set, Slice.

## Setup

Requires [uv](https://docs.astral.sh/uv/) and Python 3.12 or newer.

```sh
cd eval
uv sync
```

## Create a Principal and its API key

The admin CLI runs the same binary as the service. With Postgres from
`make dev-up` running:

```sh
# Prints the new Principal's ID.
go run ./cmd/server -config config/trelis.yaml admin principal create eval-runner

# Prints "<key-id>\t<key>". The key is stored hashed and is shown only once.
go run ./cmd/server -config config/trelis.yaml admin key create eval-runner
```

With a built binary the same commands are
`bin/trelis -config config/trelis.yaml admin ...`.

Export the key in the shell that runs the harness. It is read from
`TRELIS_API_KEY` only, never from a flag, so it stays out of shell history:

```sh
export TRELIS_API_KEY='<key>'
```

## Walkthrough against `make dev`

In one terminal:

```sh
make dev-up
make dev
```

In another, from the repository root:

```sh
# Download corpus.json and MultiHopRAG.json once; later runs skip them.
uv run --project eval trelis-eval multihop fetch

# Ingest two questions' evidence documents plus one distractor, and wait.
uv run --project eval trelis-eval multihop ingest --questions 2 --extra-docs 1 --seed 7
```

The default data directory is `eval/data/multihop-rag` (gitignored), which
assumes the commands run from the repository root. From inside `eval/`, use
`uv run trelis-eval ... --data-dir data/multihop-rag`.

The ingest command loads the Benchmark Corpus, takes the first `--questions`
Golden questions, includes every Benchmark Document they cite as evidence,
adds `--extra-docs` deterministic distractors chosen with `--seed`, uploads
each as a Document, and polls `GET /documents/{id}` until every Document is
`completed` or `failed`, or `--timeout` seconds pass. It needs
`models.embedding` configured on the Trelis side: without it, uploads parse
but every Document fails with `no embedding model is configured`. It writes:

- `subset-golden.jsonl`: the Golden Set for the selected questions.
- `manifest.jsonl`: one record per Benchmark Document, mapping `source_id`
  (its URL) to the Trelis `document_id`, with status, duplicate flag and
  error.

It prints counts by status and exits non-zero if any Document is `failed`,
`error` or `timeout`. A request that fails during upload is recorded as
`error`; a poll that fails is retried until it succeeds or `--timeout` passes.

Re-running is idempotent but not a retry. An identical upload for the same
Principal answers 409 with the earlier Document, recorded with
`"duplicate": true`. A Document that failed before is therefore reported
`failed` again rather than re-ingested, and changing `--principals` or
`--groups` on a re-run does not change the existing Document's Access List:
those fields apply only when the Document is first created.

Other commands:

```sh
# Write the full MultiHop-RAG Golden Set.
uv run --project eval trelis-eval multihop golden \
  --out eval/data/multihop-rag/golden.jsonl

# Validate a hand-edited Golden Set and print its Slice counts.
uv run --project eval trelis-eval golden validate eval/data/multihop-rag/subset-golden.jsonl
```

`--base-url` overrides the Trelis URL; without it the harness uses
`TRELIS_BASE_URL`, then `http://localhost:8080`. The Golden Set format is
documented in [docs/golden-set.md](docs/golden-set.md).

## Tests

```sh
cd eval && uv run pytest
```

or `make eval-test` from the repository root. Tests use hand-written fixtures
in `tests/fixtures/` and `httpx.MockTransport`; they need no running Trelis and
never download the real dataset.