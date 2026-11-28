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

An answer run's summary also records, per Grader, the mean over the `ok`
grades and the counts of `ok`, `grader_error` and `not_applicable`. A Grader
with no `ok` grade has a `null` mean and keeps its counts. The mean never
includes a `grader_error`, a `not_applicable` or a question that errored.

`scored` counts every answered question. In an answer run it includes a
question with no evidence, whose `metrics` is `null` because there is nothing
to compute retrieval metrics against, so `scored` can exceed the number of
observations behind the retrieval metric means.

Latency is measured under the run's `concurrency` (how many questions were in
flight at once, recorded in the header): requests that share Trelis and the
model provider wait on each other, so compare latencies only between runs at
the same concurrency. The metrics do not depend on it.

## Confidence intervals

The Ablation Matrix report gives every metric mean and both latency
percentiles a 95% confidence interval. The interval is a percentile bootstrap
over the observations that estimate uses:

- each resample draws its observations with replacement at the same size: the
  scored questions for a retrieval metric, the `ok` Grades for a Grader, and
  the scored questions' per-call `latency_ms` for latency;
- the statistic is computed on every resample: the mean for Recall@5,
  Recall@10, MRR, nDCG@10 and each Grader, and the nearest-rank percentile for
  latency;
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

## Graders

A **Grader** scores one answer on one aspect. Every Grader returns a `Grade`
with a score, a rationale and a status:

- `ok` — the Grader scored the answer; `score` is a number.
- `grader_error` — the Grader could not score it; `score` is `null`, and the
  rationale says why: a judge HTTP error, a timeout, a reply that is not the
  expected JSON, or a cited Chunk that is not among the retrieved ones.
- `not_applicable` — the answer or the Golden question gave the Grader nothing
  to check; `score` is `null`, and the rationale says what was missing: an
  empty answer, an answer that makes no factual claim, an answer that cites no
  source, or an empty expected answer.

A `grader_error` is excluded from the Grader's mean and counted. It is never
recorded as `0.0`: a judge that could not answer must not look like a wrong
answer. This is the same rule as a question-level Trelis error, and the two are
different. A Trelis error means the answer was never produced (`status:
"error"`, with the request's message); a `grader_error` means an answer exists
but one check could not score it.

A `not_applicable` is not a failure and not a zero either. Nothing failed; the
answer offered nothing to grade, so it is excluded from the mean and its
confidence interval and counted apart from `grader_error`. That keeps a judge
which keeps failing distinguishable from a run whose answers are all empty.

The registry maps a name to a Grader. `trelis-eval answer run --graders` takes
a comma list and defaults to every registered Grader, in this order:
`correctness`, `faithfulness` and `citation_precision`. An unknown name fails
before any request and lists the known names. `eval/src/trelis_eval/graders.py`
holds the registry and the Graders.

A judge call is retried up to three attempts on a 429, a 5xx or a transport
failure, with exponential backoff and jitter. A `Retry-After` of up to a
minute is honoured, a longer one stops the retries, and any other 4xx or a
2xx reply that cannot be read is not retried. A failure that outlasts the
attempts is still a `grader_error`.

Every judge-based Grader parses a reply the same way: a Markdown JSON code
fence is stripped, the body must be one JSON object, extra keys are ignored,
and the Grader validates the fields it needs. A reply that is not JSON, is not
an object or has the wrong shape is a `grader_error`, with the reply cut to a
short excerpt in the rationale.

### Correctness

`correctness` compares the answer with the expected answer. It first
normalizes both texts: NFKC, casefold, source markers such as `[1]` removed,
punctuation turned into spaces, then whitespace collapsed and stripped. If the
normalized texts are equal, the score is `1.0` with rationale `normalized
match`, and the judge is not called.

Otherwise a judge model compares the answer with the expected answer and
replies with JSON only: `{"correct": true|false, "rationale": "..."}`. A reply
wrapped in a Markdown JSON code fence is accepted; anything else is a
`grader_error`, as is a JSON object with the wrong shape. `true` scores `1.0`
and `false` scores `0.0`; the judge's rationale is kept.

An empty expected answer (empty, or only whitespace) is `not_applicable` with
rationale `no expected answer`, and the judge is not called: a gap in the
Golden Set is not a judge failure.

### The sources the citation Graders see

`faithfulness` and `citation_precision` both read a citation's text from the
retrieved Chunk with the same `chunk_id` in the answer's debug payload: the
Chunk's `context_header`, a blank line, then its `content`, or the content
alone when the header is empty. With parent-child retrieval the content is the
Parent Chunk's, which is what the generator saw, so that is what the judge
grades against. Each source is numbered by its citation `marker`, so the judge
sees `[2] ...` exactly as the answer cites `[2]`. A citation whose `chunk_id`
is not among the retrieved Chunks is a `grader_error` naming the marker, and
the judge is not called.

### Faithfulness

`faithfulness` scores the share of the answer's factual claims that the sources
it cites support.

- An empty or whitespace-only answer is `not_applicable` with rationale
  `empty answer`, and the judge is not called.
- Otherwise the judge is asked once, with the question, the answer and the
  cited sources numbered by marker. With no citations there is no source to
  show, and the judge marks every claim unsupported. The reply is JSON only:
  `{"claims": [{"claim": "...", "supported": true}], "rationale": "..."}`.
- A reply with zero claims, such as a refusal ("Insufficient information"), is
  `not_applicable` with rationale `answer makes no factual claim`.
- Otherwise the score is the number of supported claims divided by the number
  of claims. The rationale is derived: `3 of 4 claims supported;
  unsupported: "<first unsupported claim, cut to about 100 characters>"`,
  followed by the judge's rationale.

### Citation precision

`citation_precision` scores the share of cited markers whose source supports
the statement the marker is attached to.

- An answer with no citations is `not_applicable` with rationale `answer cites
  no source`, and the judge is not called.
- Otherwise the judge is asked once with the answer and the cited sources
  numbered by marker. It decides, for each cited marker, whether that source
  supports the statement the marker is attached to, and replies JSON only:
  `{"citations": [{"marker": 1, "supported": true}], "rationale": "..."}`.
- The reply must name each distinct cited marker exactly once. A missing
  marker, an extra marker or a repeated marker is a `grader_error`. A marker
  cited several times in the answer is judged once.
- The score is the number of supported markers divided by the number of
  distinct cited markers.

The judge is configured separately from the answer generator. The harness reads
`TRELIS_EVAL_JUDGE_BASE_URL`, `TRELIS_EVAL_JUDGE_MODEL` and
`TRELIS_EVAL_JUDGE_API_KEY`; `--judge-base-url` and `--judge-model` override the
first two, and the key is never a flag. The run header records the judge model.
The judge is an OpenAI-compatible Chat Completions call with `temperature: 0`,
made from the harness rather than Trelis, so the generator's `models.chat` and
the judge cannot be confused. The harness cannot check that the two are
different models, because Trelis does not expose its generator model; choosing
a different one is the operator's job. In CI the judge is one more
record/replay proxy role.

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
nDCG@10, one column per Grader present in the report, latency p50 and p95,
each as `point [low, high]`, then the scored, skipped and error counts and the
run's `concurrency`. The Grader columns are the names in the report's answer
runs, in registry order, with unknown names sorted after them; each column
uses the Grader's label. A Grader cell is the mean over its `ok` scores with
the same seeded bootstrap interval, followed by its `grader_error` and
`not_applicable` counts, each part only when it is non-zero:
`0.720 [0.660, 0.780] · 3 errors · 5 n/a` (`1 error` singular). A row that
lacks that Grader, and a retrieval row, shows `—`; an answer row with the
Grader but no `ok` grade shows `—` for the estimate plus the non-zero counts,
such as `— · 5 n/a`. A report of retrieval runs alone has no Grader columns.
Both kinds can appear in one report, and the header records the Golden Set
path and SHA-256, its question count, `top_k`, the seed, the resample count and
the confidence level; a second table records each row's `run_id` and harness
version.

The files must share the Golden Set SHA-256 and `top_k`, and no two may name
the same Retrieval Config; different `run_id`s are fine, because the baselines
come from different runs. The command exits 1 with `error: ...` when they do
not. Latency is measured under the run's `concurrency`, which is shown per
row; a report mixing concurrency levels says so, because latency compares
only at the same one.

## Slices in the report

The report breaks every metric and Grader down by Slice after the main
matrix, with one table per Slice.

The Slices are the ones carried by any question of the runs in the report,
whatever its status, so a Slice whose questions all errored still gets a
table. Question-type Slices — every name that is not `hops-N` — come first,
sorted by name, then `hops-N` Slices in ascending `N`, so `hops-2` precedes
`hops-10`. Each table is headed by the Slice and the number of Golden Set
questions carrying it (`167 questions`), counted once even when several runs
in the report share the Golden Set. That count is every question carrying the
Slice, whatever its status; a row's `Scored` count is how many of them the
config actually scored.

A table has one row per Retrieval Config and the same metric and Grader
columns as the main matrix, plus a `Scored` column: the number of that
Slice's questions the config scored. Latency, the skipped and error counts and
the concurrency are left out, because a Slice is about quality, not load.

Every estimate is recomputed over the Slice's observations: Recall@5,
Recall@10, MRR and nDCG@10 over the Slice's scored questions that have
metrics, and a Grader over the `ok` Grades in the Slice, with the same
`· N errors · N n/a` counts beside it. The interval is the same seeded
percentile bootstrap, but each (config, Slice, metric) draws from its own
`random.Random(f"{seed}:{config}:{slice}:{metric}")`, so the tables are
independent of each other and of the main matrix. A config whose run has no
question in the Slice shows `—` in every cell, `Scored` included. A config
that has questions there but scored none of them shows `n/a` for a metric with
no observation and `0` scored; a Grader with no `ok` grade shows `—` plus its
non-zero counts, like the main matrix. A Slice question with no retrieval
metric, such as a `null_query` answered in an answer run, shows `n/a` because
nothing was measured, not `—`.

## Failure Attribution

For each answer Ablation Run the report adds one Failure Attribution table:
one row for `All` and one per Slice, in the Slices section's order. The
columns are `Graded`, `Correct`, `Retrieval miss`, `Generation error`,
`Citation error` and `Unattributed`; each cell is the count and its share of
that row's `Graded`, for example `12 (24%)`. `Graded` is the number of scored
questions in the row. A row with nothing graded shows `0 (—)`, because there
is no share to take.

Each scored question gets exactly one label, in this order:

1. **Unattributed** when the `correctness` Grade is missing, `grader_error`
   or `not_applicable`: there is no outcome to attribute.
2. A **wrong** answer, one whose correctness score is below 1.0, is a
   **Retrieval miss** when the question has evidence and at least one evidence
   `source_id` is absent from the question's retrieved records. The whole
   retrieved list counts, not just the first 10 ranks, because the evidence
   may sit deeper in the `top_k` candidate pool. Otherwise it is a
   **Generation error**: every evidence Document was retrieved, or the
   question has no evidence (a `null_query`), so retrieval did not starve the
   generator.
3. A **correct** answer, one whose correctness score is 1.0, is checked
   against the `citation_precision` Grade. `ok` with a score below 1.0, or
   `not_applicable` (the answer cites nothing), is a **Citation error**; `ok`
   at 1.0 is **Correct**. A missing Grade or a `grader_error` leaves it
   **Unattributed**, because the citations could not be checked.

One missing hop already counts as a retrieval miss because a multi-hop answer
needs every evidence Document: if one was never retrieved, the generator could
not have supported that part of the answer, whatever it did with the rest. An
uncited correct answer is a citation error because the answer is right but
gives the reader nothing to verify it against, which is what the
`citation_precision` Grader reports as `not_applicable`.

The labels are computed by the report from the results file's Grades, evidence
and retrieved records; the results-file schema does not change. A report with
no answer Ablation Run says so in one line instead of showing tables.

## Questions that are not scored

In a retrieval run, a `null_query` has no evidence: its expected answer is
"insufficient information", and there is no Document whose retrieval would be
right or wrong. Such a question is recorded as `skipped`: no request is made,
no metric is computed, and it is excluded from the means but counted in the
summary.

An answer run asks every question, including a `null_query`, and grades its
answer, because "insufficient information" is a real answer to score. A
scored answer with no evidence has `metrics: null` (there is nothing to
compute retrieval metrics against) and still contributes to the correctness
mean; a retrieval run never has a scored question without metrics. `skipped`
therefore stays `0` in an answer run.

A **400 or 503** answer stops that Ablation Run at once: the config is
rejected (an unimplemented Retrieval Technique or a missing embedding role),
so the run is not valid. No results file is written, the command prints
`config <name>: aborted: <message>`, continues with the next config and exits
1 at the end. This applies to retrieval and answer runs alike.

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
question is excluded from the means, and it is counted in the summary. A
Grader's own failure follows the same rule inside that Grader: excluded from
its mean and counted, never scored as zero.

Two problems stop a run before any request:

- evidence with no completed record in the ingest manifest, because the
  retrieval metrics would silently drop below the labels;
- a Golden question with a `principal`, because per-Principal runs need the
  adversarial access set (issue #55); a run currently acts as the one Principal
  whose API key it uses.

The `trelis-eval retrieval run` and `trelis-eval answer run` commands exit 1
when any Retrieval Config aborted or scored zero questions. Question errors
alone do not change the exit
code, but they are visible in the summary line (including the first recorded
error message) and the results file.

## JSONL schema, version 2

One Ablation Run writes one file at
`<out-dir>/<run-id>/<config-name>.jsonl`. Every line is one JSON object, no
blank lines, and every line carries `"schema_version": 2` and a `"type"`:

1. one `run` header;
2. one `question` per Golden question, in Golden Set order;
3. one `summary`.

`trelis-eval` writes the file and `load_ablation_run` reads it. The reader
validates the schema version, the record types and their order, and the field
types, so a later ticket can build reports on the file without guessing. A
version-1 file has no `kind`, `judge_model`, `graders`, answer fields or
summary `grades`; the reader loads it as a retrieval run. The four files in
`eval/baselines/` are version 1, and `trelis-eval report` keeps reading them.

The header carries the SHA-256 of the Golden Set and manifest so a results
file can be tied to the exact inputs. `kind` is `retrieval` for a file written
by `retrieval run` and `answer` for one written by `answer run`. An answer
header also records the judge model (null when no judge-based Grader ran) and
the Grader names. `config.name` is the `+`-joined techniques in sorted order,
and the reader rejects a name that is not that canonical join. No file carries
an API key or the judge's key, and a question line never carries Chunk
`content` or `context_header`: results files are versioned and shared, while
text can be re-read from Trelis. The `evidence` list is de-duplicated
`source_id` values; `retrieved` follows the API's result order. Each scored
question keeps the API's `trace`, so latency can be attributed per Retrieval
Technique and model without a schema change.

### `run`

A retrieval run:

```json
{"schema_version": 2, "type": "run", "kind": "retrieval", "run_id": "20260101T000000Z", "config": {"name": "dense", "techniques": ["dense"]}, "top_k": 10, "concurrency": 1, "golden_set": {"path": "eval/data/multihop-rag/subset-golden.jsonl", "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "questions": 500}, "manifest": {"path": "eval/data/multihop-rag/manifest.jsonl", "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}, "base_url": "http://localhost:8080", "harness_version": "0.1.0", "started_at": "2026-01-01T00:00:00+00:00"}
```

An answer run adds `judge_model` and `graders`:

```json
{"schema_version": 2, "type": "run", "kind": "answer", "run_id": "20260101T000000Z", "config": {"name": "dense", "techniques": ["dense"]}, "top_k": 10, "concurrency": 1, "golden_set": {"path": "eval/data/multihop-rag/subset-golden.jsonl", "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "questions": 500}, "manifest": {"path": "eval/data/multihop-rag/manifest.jsonl", "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}, "base_url": "http://localhost:8080", "harness_version": "0.1.0", "started_at": "2026-01-01T00:00:00+00:00", "judge_model": "judge-model", "graders": ["correctness"]}
```

### `question` — retrieval, scored

```json
{"schema_version": 2, "type": "question", "id": "mhr-0000", "slices": ["comparison_query", "hops-2"], "status": "scored", "evidence": ["https://example.com/alpha", "https://example.com/beta"], "retrieved": [{"chunk_id": "11111111-1111-1111-1111-111111111111", "document_id": "22222222-2222-2222-2222-222222222222", "source_id": "https://example.com/alpha", "score": 0.82, "scores": {"dense": 0.82}}, {"chunk_id": "33333333-3333-3333-3333-333333333333", "document_id": "44444444-4444-4444-4444-444444444444", "source_id": null, "score": 0.77, "scores": {"dense": 0.77}}, {"chunk_id": "55555555-5555-5555-5555-555555555555", "document_id": "66666666-6666-6666-6666-666666666666", "source_id": "https://example.com/beta", "score": 0.71, "scores": {"dense": 0.71}}], "trace": [{"step": "embed_query", "technique": "dense", "duration_ms": 3, "model": "text-embedding-3-small"}, {"step": "search", "technique": "dense", "duration_ms": 6, "candidates": 3}], "metrics": {"recall@5": 1.0, "recall@10": 1.0, "mrr": 1.0, "ndcg@10": 0.9197207891481876}, "latency_ms": 12.4}
```

The unmapped Chunk at rank 2 stays in the ranked list as `null` and occupies a
rank, but it is never relevant. The two evidence Documents at ranks 1 and 3
give nDCG@10 = `(1/log2(2) + 1/log2(4)) / (1/log2(2) + 1/log2(3))`.

### `question` — answer, scored

```json
{"schema_version": 2, "type": "question", "id": "mhr-0000", "slices": ["comparison_query", "hops-2"], "status": "scored", "evidence": ["https://example.com/alpha"], "retrieved": [{"chunk_id": "11111111-1111-1111-1111-111111111111", "document_id": "22222222-2222-2222-2222-222222222222", "source_id": "https://example.com/alpha", "score": 0.82, "scores": {"dense": 0.82}}], "trace": [{"step": "embed_query", "technique": "dense", "duration_ms": 3, "model": "text-embedding-3-small"}, {"step": "search", "technique": "dense", "duration_ms": 6, "candidates": 1}, {"step": "generate", "technique": null, "duration_ms": 900, "model": "chat-model"}], "metrics": {"recall@5": 1.0, "recall@10": 1.0, "mrr": 1.0, "ndcg@10": 1.0}, "latency_ms": 1004.2, "answer": "The Alpha Mission launched in 2024 [1].", "citations": [{"marker": 1, "chunk_id": "11111111-1111-1111-1111-111111111111", "document_id": "22222222-2222-2222-2222-222222222222"}], "finish_reason": "stop", "usage": {"prompt_tokens": 1200, "completion_tokens": 40, "total_tokens": 1240}, "grades": {"correctness": {"score": 1.0, "rationale": "normalized match", "status": "ok"}}}
```

A `grader_error` entry carries `"score": null`, the reason in `rationale` and
`"status": "grader_error"`; a `not_applicable` entry looks the same with
`"status": "not_applicable"`. A scored answer with no evidence has `"metrics":
null` and still carries its answer and grades.

### `question` — skipped and error

```json
{"schema_version": 2, "type": "question", "id": "mhr-0002", "slices": ["null_query", "hops-0"], "status": "skipped", "evidence": [], "retrieved": [], "trace": [], "metrics": null, "latency_ms": 0.0}
```

```json
{"schema_version": 2, "type": "question", "id": "mhr-0003", "slices": ["temporal_query", "hops-1"], "status": "error", "evidence": ["https://example.com/delta"], "retrieved": [], "trace": [], "metrics": null, "latency_ms": 5.2, "error": "HTTP 500 (code 1000): internal server error"}
```

`metrics` is an object for a scored retrieval question, and may be `null` for
a scored answer question with no evidence. `error` appears only on `error`;
`trace` is empty on `skipped` and `error`. An answer file's skipped and error
records also carry `"answer": null`, `"citations": []`,
`"finish_reason": null`, `"usage": null` and `"grades": {}`; only a scored
answer has values there. A retrieval file's question records carry none of
those five fields.

### `summary`

```json
{"schema_version": 2, "type": "summary", "metrics": {"recall@5": 0.9975, "recall@10": 1.0, "mrr": 0.9932, "ndcg@10": 0.9411}, "counts": {"scored": 497, "skipped": 3, "error": 0}, "latency_ms": {"p50": 11.8, "p95": 34.2}, "grades": {"correctness": {"mean": 0.72, "ok": 8, "grader_error": 1, "not_applicable": 0}}, "finished_at": "2026-01-01T00:10:00+00:00"}
```

A retrieval run omits `grades` entirely; the reader rejects it there, just as
it rejects answer-only fields in a retrieval header or question. An answer run
writes one entry per Grader it ran: the mean over the `ok` scores (`null` when
there is none) and the `ok`, `grader_error` and `not_applicable` counts.
Version 2 was released together with the Grader fields, so no published version
2 file should lack `not_applicable`; a file the #48 harness wrote still loads,
with a count of 0. A harness older than this change rejects the
`not_applicable` status, so a file written now does not load there.