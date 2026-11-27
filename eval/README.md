# Trelis eval harness

A separate Python program that measures Trelis from the outside
([ADR-0001](../docs/adr/0001-external-python-eval-harness.md)). It talks to
a running Trelis through the public HTTP API as one Principal, with that
Principal's API key, so every run exercises the same path a real client takes.
It never imports Go code, reads Postgres or calls the admin CLI; results are
versioned files, not server state.

The harness fetches the MultiHop-RAG Benchmark Corpus, ingests a subset as
Documents, waits for ingest to finish, reads and writes Golden Sets, and
scores Ablation Runs under Retrieval Configs with Recall@5, Recall@10, MRR and
nDCG@10. It also runs an OpenAI-compatible record/replay proxy so paid model
calls are recorded once and replayed for free.

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
Golden questions (or a stratified sample, see below), includes every Benchmark
Document they cite as evidence, adds `--extra-docs` deterministic distractors
chosen with `--seed`, uploads each as a Document, and polls
`GET /documents/{id}` until every Document is `completed` or `failed`, or
`--timeout` seconds pass. It needs `models.embedding` configured on the Trelis
side: without it, uploads parse but every Document fails with
`no embedding model is configured`. It writes:

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

### Stratified subset

`--sample stratified` allocates `--questions` across the questions'
`question_type` Slices in proportion to the whole Benchmark Corpus, using the
largest remainder method and then sampling each Slice with `--seed`. The
default is `--sample first`, which keeps the earlier first-questions behaviour.

For the full 2,556-question MultiHop-RAG set, 500 questions allocate as
comparison 167, inference 160, temporal 114 and null 59:

```sh
uv run --project eval trelis-eval multihop ingest \
  --sample stratified --questions 500 --seed 1
```

The written `subset-golden.jsonl` is the sample; the evidence Documents and
`--extra-docs` distractors are selected exactly as for the first-questions
subset.

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

## Run a retrieval ablation

After an ingest has completed, score the subset Golden Set under one or more
Retrieval Configs:

```sh
uv run --project eval trelis-eval retrieval run \
  --golden eval/data/multihop-rag/subset-golden.jsonl \
  --manifest eval/data/multihop-rag/manifest.jsonl
```

Pass `--config` more than once to compare configs in one command, for example
`--config dense --config bm25+dense` once the server implements `bm25` (#33).
Each value is Retrieval Techniques joined with `+`; techniques are sorted, so
`dense+bm25` and `bm25+dense` are the same config, and that canonical name is
also the results file name. The default is one `dense` config. `--top-k` must
be between 10 and 100; the header records it, and MRR is MRR@top-k in effect.

Each `POST /retrieve` includes Trelis's own call to the embedding model, so a
slow hosted provider sets the pace. `--request-timeout` (default 120 s) keeps a
slow answer from becoming a question error, and `--concurrency N` (default 1,
at most 32) keeps N questions in flight to cut the wall-clock time. The header
records the concurrency because latency is measured under it.

Under each summary line, `steps:` gives the p50/p95 of every `/retrieve` trace
step (for example `dense/embed_query` and `dense/search`), so a slow run shows
which step is slow rather than only the request total.

The command runs the configs one after another, writes
`eval/runs/<run-id>/<config-name>.jsonl`, and prints one line per Ablation Run
with `scored`/`skipped`/`error` counts, the four metric means and the p50/p95
latency, followed by the file path. The run id defaults to a UTC timestamp
`YYYYMMDDTHHMMSSZ` and is shared by every config of one command; `--run-id`
fixes it. The command refuses to replace an existing results file, so a reused
run id must be one that has not run yet.

`recall@5`, `recall@10`, `mrr` and `ndcg@10` are Document-level, and the JSONL
schema is documented in [docs/metrics.md](docs/metrics.md). `eval/runs/` is
gitignored. The command exits 1 when a config aborted or scored no questions. A
400 or 503 aborts that config at once (no file is written) and the command
continues with the next one; any other request error is recorded on the
question and excluded from the means, not scored as zero. Errors alone do not
change the exit code.

## Record and replay model calls

Trelis calls its providers inside the Go service, so a retrieval run pays for
an embedding on every question. Start the proxy and point the embedding role
at it to pay once:

```sh
uv run --project eval trelis-eval proxy --mode record \
  --upstream embedding=https://openrouter.ai/api/v1 \
  --fixtures eval/fixtures/models
```

The command prints the `TRELIS_MODELS_EMBEDDING_BASE_URL` line to start Trelis
with. Run the same Ablation Run again with `--mode replay` (no upstream and no
provider key) and every recorded answer is served from
`eval/fixtures/models` byte for byte, including its original `usage`. A
missing fixture fails with a `404` and names the request, so a prompt or
Chunk change cannot silently score a stale answer. [docs/record-replay.md](docs/record-replay.md)
covers the routing, the key, the fixture schema, the modes, the miss workflow
and a worked dense-baseline example.

## Re-run a baseline

`scripts/eval-stack.sh` replaces `make dev` for an Ablation Run. It builds
Trelis from the checkout and starts it on `:8080` against the dev database,
with the embedding role routed through the proxy, and keeps both running in
the background:

```sh
# First run of a new question set: pay each query embedding once.
TRELIS_MODELS_EMBEDDING_API_KEY=... scripts/eval-stack.sh up record
# Every later run: fixtures only, no provider key, no cost.
scripts/eval-stack.sh up replay

uv run --project eval trelis-eval retrieval run \
  --golden eval/data/multihop-rag/strat500-golden.jsonl \
  --manifest eval/data/multihop-rag/strat500-manifest.jsonl \
  --config bm25 --config dense --config bm25+dense --concurrency 4

scripts/eval-stack.sh down
```

The stratified-500 corpus in the dev database belongs to the Principal
`eval`, so the run needs one of its keys (`admin key create eval`, see
above). Stop `make dev` first: the script refuses to start while something
serves `:8080`. A replay run reproduces a recorded run's metrics exactly, and
its `dense/embed_query` step drops to a few milliseconds, so compare latency
only between runs that made, or replayed, the same model calls.
`eval/baselines/README.md` lists the kept reference rows and the command for
each.

## Tests

```sh
cd eval && uv run pytest
```

or `make eval-test` from the repository root. Tests use hand-written fixtures
in `tests/fixtures/` and `httpx.MockTransport`; they need no running Trelis and
never download the real dataset.