# Retrieval metrics and Ablation Run results

One Ablation Run scores a Golden Set under one Retrieval Config through
`POST /retrieve`. This document defines the metrics and the versioned JSONL
file each run writes. The terms are the ones in
[GLOSSARY.md](../../GLOSSARY.md).

## Relevance is at Document level

MultiHop-RAG labels evidence per article, and `trelis-eval multihop ingest`
turns each article into one Trelis Document. A Document can produce many
Chunks, so scoring Chunks would reward a parser for splitting one article into
more pieces. The metrics therefore treat a Document as relevant once, however
many of its Chunks are retrieved.

The ranked list a metric sees is the `source_id` of each retrieved Chunk, in
the order the API returned it. `source_id` comes from the ingest manifest's
`document_id → source_id` mapping, completed records only. A Chunk whose
Document has no completed manifest record maps to `None` and is never
relevant. The relevant collection is the set of distinct `source_id` values in
the Golden question's `evidence`; several facts may cite the same article.

Formulas below use:

- `ranked` — the `source_id` or `None` per retrieved Chunk, in rank order;
- `R` — the distinct relevant `source_id` values;
- `k` — the cutoff in ranks, 1-based.

Every metric raises `ValueError` for an empty `R` (a question with no evidence
is skipped before scoring) and, where it has a cutoff, for `k < 1`.

### Recall@k

```
Recall@k = |{source_id in ranked[1:k]} ∩ R| / |R|
```

Each relevant Document counts once even if several of its Chunks are in the
top `k`. A `k` larger than the ranked list is allowed: the whole list is used,
which is the natural reading of "retrieved so far".

### MRR

```
RR = 1 / rank of the first relevant entry   (0 if none)
MRR = mean of RR over the scored questions
```

Only the first relevant Chunk matters. A `None` entry still occupies a rank.
MRR is computed over the ranks `top_k` returned, so it is MRR@`top_k` in
effect. A run requires `10 <= top_k <= 100`; the header records the exact
`top_k`, and MRR from runs with different `top_k` values is not comparable.

### nDCG@10

Gain is binary. A relevant `source_id` gains once, at its first occurrence;
later Chunks of the same Document gain 0, so one Document cannot be counted
twice. A non-relevant or `None` entry gains 0.

```
DCG@k  = Σ(i = 1..k) gain_i / log2(i + 1)
IDCG@k = Σ(i = 1..min(|R|, k)) 1 / log2(i + 1)
nDCG@k = DCG@k / IDCG@k
```

The `IDCG` is the best possible ordering: every relevant Document at the top,
one gain each. If the ranked list is shorter than `k`, the missing ranks gain
0, as they would for any list.

The hand-computed test case in `eval/tests/test_metrics.py` is ranked
`[a, x, b, a, None]` with `R = {a, b, c}`: Recall@1 = 1/3, Recall@5 = 2/3,
RR = 1, and nDCG@5 =
`(1/log2(2) + 1/log2(4)) / (1/log2(2) + 1/log2(3) + 1/log2(4))` ≈ 0.7039.
The second `a` at rank 4 gains 0.

## Summary

The summary averages each metric over the **scored** questions only. It also
records the counts and the latency p50/p95 of the scored calls. Latency uses
the nearest-rank definition: sort the latencies and take rank
`ceil(p * n / 100)`, computed with integers. When no question was scored, every
mean and both latencies are `null`, not `0`, so "nothing measured" is not
mistaken for a score of zero.

Latency is measured under the run's `concurrency` (how many questions were in
flight at once, recorded in the header): requests that share Trelis and the
model provider wait on each other, so compare latencies only between runs at
the same concurrency. The metrics do not depend on it.

## Confidence intervals

The Ablation Matrix report gives every metric mean and both latency
percentiles a 95% confidence interval. The interval is a percentile bootstrap
over the questions that were scored:

- each resample draws the scored questions with replacement at the same size
  as the scored set; a metric's per-question value is one observation, and the
  latency percentiles use the per-call `latency_ms` of the same questions;
- the statistic is computed on every resample: the mean for Recall@5,
  Recall@10, MRR and nDCG@10, and the nearest-rank percentile for latency;
- the resample statistics are sorted, and the bounds are the values at rank
  `ceil(q * resamples)` (1-based, at least 1) for `q = 0.025` and
  `q = 0.975`, computed with exact integer arithmetic. The tail is rounded
  once to basis points, so the 0.95 default is exactly 2.5% and not the
  nearest binary float. This is the same nearest-rank rule as the latency
  summary;
- the default is 10,000 resamples with seed 1. A row with no scored question
  shows `n/a` for every metric and both latencies, never `0`; constant values
  give the value at both bounds.

The bootstrap is seeded, and each (row, metric) draws from its own
`random.Random(f"{seed}:{config_name}:{metric}")`, so adding, removing or
reordering rows leaves another row's interval unchanged. The same results
files and seed give byte-identical Markdown and HTML. The interval describes
the sampled Golden questions, not a rerun of the same questions.

## The Ablation Matrix report

`trelis-eval report` turns results files into the Ablation Matrix:

```sh
uv run --project eval trelis-eval report eval/baselines
```

`RESULTS` is one or more results files; a directory contributes every
`*.jsonl` in it, sorted by name. The command writes
`<out-dir>/ablation-matrix.md` and `<out-dir>/ablation-matrix.html` (default
`eval/reports`) and prints both paths. It reads the files only: no HTTP
request and no API key. Nothing in the report depends on the wall clock, so
the same files and seed reproduce it byte for byte.

The matrix has one row per Retrieval Config: Recall@5, Recall@10, MRR,
nDCG@10, latency p50 and p95, each as `point [low, high]`, then the scored,
skipped and error counts and the run's `concurrency`. The header records the
Golden Set path and SHA-256, its question count, `top_k`, the seed, the
resample count and the confidence level; a second table records each row's
`run_id` and harness version.

The files must share the Golden Set SHA-256 and `top_k`, and no two may name
the same Retrieval Config; different `run_id`s are fine, because the baselines
come from different runs. The command exits 1 with `error: ...` when they do
not. Latency is measured under the run's `concurrency`, which is shown per
row; a report mixing concurrency levels says so, because latency compares
only at the same one.

## Questions that are not scored

A `null_query` has no evidence: its expected answer is "insufficient
information", and there is no Document whose retrieval would be right or
wrong. Such a question is recorded as `skipped`: no request is made, no metric
is computed, and it is excluded from the means but counted in the summary.

A **400 or 503** answer stops that Ablation Run at once: the config is
rejected (an unimplemented Retrieval Technique or a missing embedding role),
so the run is not valid. No results file is written, the command prints
`config <name>: aborted: <message>`, continues with the next config and exits
1 at the end.

**10 question errors in a row** also stop that config's Ablation Run, because
the requests are failing rather than the questions: the provider is refusing
the calls, or the service is down. `--max-consecutive-errors N` sets the limit
(default 10; 0 disables the rule). A skipped question neither resets nor
advances the streak; a scored question resets it. With `--concurrency` above 1
the streak follows the order results arrive, not Golden Set order, and no
further question is sent once it aborts. The abort is the 400/503 abort in
every other respect: no results file, continue with the next config, exit 1 at
the end. The command prints
`config <name>: aborted: <N> consecutive question errors; last: <message>`.

Every other request failure (a 5xx other than 503, or an `httpx.HTTPError`) is
recorded on that question as `error` with the message. A **502** is one of
these: Trelis answers it when a model provider rejects the call
(`ErrModelProvider`, a provider status 401, 402, 403 or 429), and the message
names the model role rather than the provider's body. One such error is a
question error like any other; repeated, it trips the consecutive rule above
and aborts that config's Ablation Run. The run continues otherwise, the
question is excluded from the means, and it is counted in the summary. This is
the "grader errors are excluded and counted" rule: a broken question never
silently becomes a zero.

Two problems stop a run before any request:

- evidence with no completed record in the ingest manifest, because recall
  would silently drop below the labels;
- a Golden question with a `principal`, because per-Principal runs need the
  adversarial access set (issue #55); a run currently acts as the one Principal
  whose API key it uses.

The `trelis-eval retrieval run` command exits 1 when any Retrieval Config
aborted or scored zero questions. Question errors alone do not change the exit
code, but they are visible in the summary line (including the first recorded
error message) and the results file.

## JSONL schema, version 1

One Ablation Run writes one file at
`<out-dir>/<run-id>/<config-name>.jsonl`. Every line is one JSON object, no
blank lines, and every line carries `"schema_version": 1` and a `"type"`:

1. one `run` header;
2. one `question` per Golden question, in Golden Set order;
3. one `summary`.

`trelis-eval` writes the file and `load_ablation_run` reads it. The reader
validates the schema version, the record types and their order, and the field
types, so a later ticket can build reports on the file without guessing.

The header carries the SHA-256 of the Golden Set and manifest so a results
file can be tied to the exact inputs. `config.name` is the `+`-joined
techniques in sorted order, and the reader rejects a name that is not that
canonical join. It never carries an API key, and a question line never carries
Chunk `content` or `context_header`: results files are versioned and shared,
while text can be re-read from Trelis. The `evidence` list is de-duplicated
`source_id` values; `retrieved` follows the API's result order. Each scored
question keeps the `POST /retrieve` `trace`, so latency can be attributed per
Retrieval Technique and model without a schema change.

### `run`

```json
{"schema_version": 1, "type": "run", "run_id": "20260101T000000Z", "config": {"name": "dense", "techniques": ["dense"]}, "top_k": 10, "concurrency": 1, "golden_set": {"path": "eval/data/multihop-rag/subset-golden.jsonl", "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "questions": 500}, "manifest": {"path": "eval/data/multihop-rag/manifest.jsonl", "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}, "base_url": "http://localhost:8080", "harness_version": "0.1.0", "started_at": "2026-01-01T00:00:00+00:00"}
```

### `question` — scored

```json
{"schema_version": 1, "type": "question", "id": "mhr-0000", "slices": ["comparison_query", "hops-2"], "status": "scored", "evidence": ["https://example.com/alpha", "https://example.com/beta"], "retrieved": [{"chunk_id": "11111111-1111-1111-1111-111111111111", "document_id": "22222222-2222-2222-2222-222222222222", "source_id": "https://example.com/alpha", "score": 0.82, "scores": {"dense": 0.82}}, {"chunk_id": "33333333-3333-3333-3333-333333333333", "document_id": "44444444-4444-4444-4444-444444444444", "source_id": null, "score": 0.77, "scores": {"dense": 0.77}}, {"chunk_id": "55555555-5555-5555-5555-555555555555", "document_id": "66666666-6666-6666-6666-666666666666", "source_id": "https://example.com/beta", "score": 0.71, "scores": {"dense": 0.71}}], "trace": [{"step": "embed_query", "technique": "dense", "duration_ms": 3, "model": "text-embedding-3-small"}, {"step": "search", "technique": "dense", "duration_ms": 6, "candidates": 3}], "metrics": {"recall@5": 1.0, "recall@10": 1.0, "mrr": 1.0, "ndcg@10": 0.9197207891481876}, "latency_ms": 12.4}
```

The unmapped Chunk at rank 2 stays in the ranked list as `null` and occupies a
rank, but it is never relevant. The two evidence Documents at ranks 1 and 3
give nDCG@10 = `(1/log2(2) + 1/log2(4)) / (1/log2(2) + 1/log2(3))`.

### `question` — skipped and error

```json
{"schema_version": 1, "type": "question", "id": "mhr-0002", "slices": ["null_query", "hops-0"], "status": "skipped", "evidence": [], "retrieved": [], "trace": [], "metrics": null, "latency_ms": 0.0}
```

```json
{"schema_version": 1, "type": "question", "id": "mhr-0003", "slices": ["temporal_query", "hops-1"], "status": "error", "evidence": ["https://example.com/delta"], "retrieved": [], "trace": [], "metrics": null, "latency_ms": 5.2, "error": "HTTP 500 (code 1000): internal server error"}
```

`metrics` is an object only for `scored`; `error` appears only on `error`;
`trace` is empty on `skipped` and `error`.

### `summary`

```json
{"schema_version": 1, "type": "summary", "metrics": {"recall@5": 0.9975, "recall@10": 1.0, "mrr": 0.9932, "ndcg@10": 0.9411}, "counts": {"scored": 497, "skipped": 3, "error": 0}, "latency_ms": {"p50": 11.8, "p95": 34.2}, "finished_at": "2026-01-01T00:10:00+00:00"}
```