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
nDCG@10. Answer Ablation Runs also grade each answer with a Grader:
correctness, faithfulness and citation precision. It also runs an
OpenAI-compatible record/replay proxy so paid model calls are recorded once
and replayed for free.

Terms are the ones in [GLOSSARY.md](../GLOSSARY.md): Corpus, Benchmark
Corpus, Document, Principal, Access List, Golden Set, Slice, Grader.

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
  --manifest eval/data/multihop-rag/manifest.jsonl \
  --model-mode live
```

Every run needs `--model-mode` or `$TRELIS_EVAL_MODEL_MODE`; without either it
fails before its first request. The mode records where the model answers came
from: `live` means Trelis called the providers directly, and `passthrough`,
`record` and `replay` are the record/replay proxy's modes. The Ablation Matrix
reports latency only for a `live` or `passthrough` run: a `record` run mixes
recorded upstream calls with replayed ones, and a `replay` run replays every
call.

Pass `--config` more than once to compare configs in one command, for example
`--config dense --config bm25+dense` once the server implements `bm25` (#33).
Each value is Retrieval Techniques joined with `+`; techniques are sorted, so
`dense+bm25` and `bm25+dense` are the same config, and that canonical name is
also the results file name. The default is one `dense` config. `--top-k` must
be between 10 and 100; the header records it, and MRR is MRR@top-k in effect.

`--config bm25+dense+rerank` adds the rerank row: it needs `models.rerank`
configured on the server and scores the server's default pool of 50
candidates, since the harness does not send `rerank_candidates`. Start the
eval stack with `--rerank` (see [Re-run a baseline](#re-run-a-baseline)) so a
rerank call is recorded once and replayed for free afterwards.

Each `POST /retrieve` includes Trelis's own call to the embedding model, so a
slow hosted provider sets the pace. `--request-timeout` (default 120 s) keeps a
slow answer from becoming a question error, and `--concurrency N` (default 1,
at most 32) keeps N questions in flight to cut the wall-clock time. The header
records the concurrency because latency is measured under it.
`--max-consecutive-errors N` (default 10; 0 disables) aborts a config once N
questions in a row answer as errors, so a provider refusing every call stops
the run instead of filling a results file. A skipped question neither resets
nor advances the streak; a scored one resets it.

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
400 or 503 aborts that config at once (no file is written), as does reaching
`--max-consecutive-errors` question errors in a row, and the command continues
with the next one; any other request error, including a 502 from a model
provider that rejected the call, is recorded on the question and excluded from
the means, not scored as zero. Errors alone do not change the exit code.

## Run an answer ablation

An answer Ablation Run asks `POST /answer` with `debug: true`, computes the
same retrieval metrics from the retrieved Chunks, and grades every answer. It
uses the same Golden Set and manifest as a retrieval run, so ingest once and
score both.

The judge is a separate model from the answer generator. Put its settings in
`.local/eval.env` at the repository root (gitignored) and export them in the
shell that runs the harness; never write a real key into this README or any
tracked file:

```sh
# .local/eval.env
TRELIS_EVAL_JUDGE_BASE_URL=<OpenAI-compatible base URL>
TRELIS_EVAL_JUDGE_MODEL=<judge model name>
TRELIS_EVAL_JUDGE_API_KEY=<judge API key>
```

```sh
set -a; . .local/eval.env; set +a
uv run --project eval trelis-eval answer run \
  --golden eval/data/multihop-rag/subset-golden.jsonl \
  --manifest eval/data/multihop-rag/manifest.jsonl \
  --model-mode live \
  --graders correctness
```

`--judge-base-url` and `--judge-model` override the first two variables; the
key is read from `TRELIS_EVAL_JUDGE_API_KEY` only, never from a flag.
`--graders` takes a comma list and defaults to every registered Grader:
`correctness`, `faithfulness` and `citation_precision`. A default answer run
therefore makes up to three judge calls per question, one per judge-based
Grader; `--graders correctness` makes at most one.
[docs/metrics.md](docs/metrics.md#graders) defines the three. `correctness`
scores a normalized match 1 without a judge call. `faithfulness` scores the
share of the answer's claims its cited Chunks support, and
`citation_precision` the share of cited markers whose Chunk supports the
statement the marker is attached to. Both read a cited Chunk's
`context_header` and content from the answer's debug payload, and both are
`not_applicable` rather than a zero when the answer gives them nothing to
check. A judge that cannot answer is a `grader_error`, excluded from the mean
and counted apart from `not_applicable`, never a zero.

The other options are `retrieval run`'s: `--config` (repeatable), `--top-k`,
`--concurrency`, `--max-consecutive-errors`, `--request-timeout`, `--out-dir`,
`--run-id` and `--base-url`. A null query is still asked and graded; it just
has no retrieval metric. The results file records the judge model and the
Grader names, and the summary line adds each Grader's mean and counts.

To record and replay the judge too, point its base URL at the proxy: pass
`--upstream judge=<provider URL>` in `record` mode and set
`TRELIS_EVAL_JUDGE_BASE_URL=http://127.0.0.1:4100/judge` in both modes. The
proxy already routes any `[a-z]+` role, so it needs no change. The harness
still requires a non-empty `TRELIS_EVAL_JUDGE_API_KEY` in replay, because it
always sends an `Authorization` header; the proxy ignores its value when no
upstream is called.

## Build the Ablation Matrix report

Turn results files into the Ablation Matrix, with a 95% confidence interval on
every metric, each Grader column, every latency percentile, Tokens and Cost
per Correct Answer:

```sh
uv run --project eval trelis-eval report eval/baselines
```

`report` accepts results files or a directory of `*.jsonl` (sorted by name),
writes `eval/reports/ablation-matrix.md` and `ablation-matrix.html`, and prints
both paths. `--seed` (default 1) and `--resamples` (default 10,000) control the
bootstrap; `--out-dir` chooses another directory. `--prices PATH` names the
price table (default `eval/prices.toml` when it exists; a missing file given
explicitly is an error). It needs no running Trelis. The report also has one
table per Slice and, for answer runs, a Failure Attribution table. Latency is
shown only for `live` and `passthrough` runs: a `record` run mixes recorded
upstream calls with replayed ones, a `replay` run replays every call, and a
results file from before model modes shows `unknown`, with a note under the
matrix. Tokens and Cost per Correct Answer are `—` on a
retrieval row; a model the price table does not price also leaves that row's
cost `—`, with a note naming the model. `eval/reports/` is meant to be
published, not gitignored. The interval, comparability and determinism rules
are in [docs/metrics.md](docs/metrics.md#confidence-intervals); Tokens and Cost
per Correct Answer are defined in
[Tokens and Cost per Correct Answer](docs/metrics.md#tokens-and-cost-per-correct-answer),
and the two older sections in [Slices in the report](docs/metrics.md#slices-in-the-report)
and [Failure Attribution](docs/metrics.md#failure-attribution).

### Fill in the price table

`eval/prices.toml` is committed with a currency and the price of the chat
model this repo's eval stack uses (`deepseek-v4-flash-0731`, with its source
and date). An answer whose model is not in the table makes Cost per Correct
Answer `—` for that row. Add one table per model, keyed by the `model` of the
answer's `generate` trace step exactly as Trelis reports it:

```toml
currency = "USD"

[models."deepseek-v4-flash-0731"]
input_per_million = 0.27
output_per_million = 1.10
```

Those numbers are only the shape, not a quote; use what your provider charges.
Prices are per million tokens and must not be negative. Cost covers only the
generate call, because Trelis's debug object reports only the chat call's
usage: embedding, rerank and the judge are not included.

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
with. Run the Ablation Run with `--model-mode record`; the replay pass uses
`--model-mode replay`. Run the same Ablation Run again with `--mode replay` (no
upstream and no provider key) and every recorded answer is served from
`eval/fixtures/models` byte for byte, including its original `usage`. A
missing fixture fails with a `404` and names the request, so a prompt or
Chunk change cannot silently score a stale answer. [docs/record-replay.md](docs/record-replay.md)
covers the routing, the key, the fixture schema, the modes, the miss workflow
and a worked dense-baseline example.

## Re-run a baseline

`scripts/eval-stack.sh` replaces `make dev` for an Ablation Run. It builds
Trelis from the checkout and starts it on `:8080` against the dev database,
with the embedding role routed through the proxy, and keeps both running in
the background. Add `--rerank` to route the rerank role too, so a
`bm25+dense+rerank` run records and replays its rerank calls as well:

```sh
# First run of a new question set: pay each query embedding once. With
# --rerank, each rerank call is paid once too.
TRELIS_MODELS_EMBEDDING_API_KEY=... TRELIS_MODELS_RERANK_API_KEY=... \
  scripts/eval-stack.sh up record --rerank
# Every later run: fixtures only, no provider keys, no cost.
scripts/eval-stack.sh up replay --rerank

uv run --project eval trelis-eval retrieval run \
  --golden eval/data/multihop-rag/strat500-golden.jsonl \
  --manifest eval/data/multihop-rag/strat500-manifest.jsonl \
  --model-mode replay \
  --config bm25 --config dense --config bm25+dense --config bm25+dense+rerank \
  --concurrency 4

scripts/eval-stack.sh down
```

`up` prints the `export TRELIS_EVAL_MODEL_MODE=<mode>` line to use: `record`
with `up record`, `replay` with `up replay`. The run must record the mode it
actually used, because the report shows latency only for `live` and
`passthrough` runs. The command above uses the replay stack; pass
`--model-mode record` (or export it) for the recording run.

The script exports `.local/models.env` when it exists, so the keys above can
live there instead. Unlike `make dev`, where the file overrides the
environment, a variable already in the environment wins here. Without
`--rerank`, it warns when the rerank role is configured (in
`config/trelis.yaml` or `TRELIS_MODELS_RERANK_BASE_URL`), because those calls
would reach the provider unrecorded.

Before it starts the proxy or Trelis, `up` prints one line per routed role
(embedding, and rerank with `--rerank`) with the model Trelis will send and
how many fixtures already answer that role and model. The model comes from
`TRELIS_MODELS_<ROLE>_MODEL` when set, otherwise the role's `model:` under
`models:` in `config/trelis.yaml`, and `model: unknown` when neither names
one. In record mode it warns when a role has fixtures but none for that
model, because every call for the new model will then be paid; in replay
mode it warns that every call will instead miss as a missing fixture. The
same summary is available on its own with
`uv run --project eval trelis-eval proxy fixtures --fixtures eval/fixtures/models`.

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