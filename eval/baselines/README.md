# Baselines

Ablation Run files kept in the repo as reference points: each is the
unchanged JSONL that `trelis-eval retrieval run` wrote (schema in
[`../docs/metrics.md`](../docs/metrics.md)). They hold question IDs, evidence
URLs, Chunk and Document IDs, scores and the `/retrieve` trace. They hold no
question text, Chunk content or API key.

Document and Chunk IDs are those of the Trelis that produced the run, so they
mean nothing to another instance. Compare the scores, not the IDs.

## `dense-strat500-seed1.jsonl`

The first row of the Ablation Matrix: dense retrieval alone.

| Metric | Value (n = 441 scored) |
|---|---|
| Recall@5 | 0.630 |
| Recall@10 | 0.779 |
| MRR | 0.797 |
| nDCG@10 | 0.678 |
| Latency p50 / p95 | 23.9 s / 34.5 s at concurrency 4 |

The 59 `null_query` questions are skipped, with 0 errors. Run
`uv run --project eval trelis-eval report eval/baselines` for the 95%
confidence interval on every value.

How it was produced (2026-11-27, Trelis `6beccaa`, harness 0.1.0):

- **Corpus:** all 609 MultiHop-RAG documents. That is the 320 cited by the
  sample plus the 289 others as distractors, so the search runs over the
  whole Benchmark Corpus. Ingested as Markdown with the default chunker (512
  tokens, 80 overlap): 16,920 Chunks.
- **Embedding:** `qwen/qwen3-embedding-8b` through OpenRouter (4,096 dims).
  The search is an exact cosine scan.
- **Golden Set:** 500 questions, stratified by question type with seed 1
  (comparison 167, inference 160, temporal 114, null 59). Its sha256 is in the
  header. Regenerate it, and check the hash, with:

  ```sh
  uv run --project eval trelis-eval multihop ingest --sample stratified \
    --questions 500 --seed 1 --extra-docs 289 \
    --golden-out eval/data/multihop-rag/strat500-golden.jsonl \
    --manifest-out eval/data/multihop-rag/strat500-manifest.jsonl
  uv run --project eval trelis-eval retrieval run \
    --golden eval/data/multihop-rag/strat500-golden.jsonl \
    --manifest eval/data/multihop-rag/strat500-manifest.jsonl \
    --config dense --concurrency 4
  ```

- **Latency:** the search step dominated the `/retrieve` trace (p50 21 s,
  max 26 s), not the hosted embedding call (p50 2.5 s). The search saw a
  Postgres generic plan that re-parsed the query vector once per Chunk;
  #143 has the plan and timing numbers. Compare runs only at the same
  concurrency and provider.

## `bm25-strat500-seed1.jsonl` and `bm25+dense-strat500-seed1.jsonl`

The BM25 row (#33) and the hybrid row, BM25 and dense fused with Reciprocal
Rank Fusion at `rrf_k` 60 (#34). Both come from one command that also re-ran
dense, so the three rows below share one Trelis, one corpus and one Golden
Set.

| Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Latency p50 / p95 |
|---|---|---|---|---|---|
| `bm25` | 0.621 | 0.744 | 0.780 | 0.659 | 21 ms / 29 ms |
| `dense` (same command, not kept) | 0.629 | 0.781 | 0.796 | 0.679 | 2.1 s / 6.1 s |
| `bm25+dense` | 0.666 | 0.811 | 0.828 | 0.714 | 244 ms / 259 ms |

n = 441 scored for each, with the 59 `null_query` questions skipped and 0
errors. Run `uv run --project eval trelis-eval report eval/baselines` for the
95% confidence interval on every value.

The re-run dense row matches `dense-strat500-seed1.jsonl` to within 0.002 on
every metric, which is why that file stays the dense reference.

How they were produced (2026-11-27, Trelis `d4b4c0b`, harness 0.1.0):

- **Corpus, Golden Set and embedding:** the same as the dense baseline above,
  on the same Trelis instance and the same 16,920 Chunks. BM25 indexes them
  with pg_search 0.27.1 (ADR-0009).
- **Command:**

  ```sh
  uv run --project eval trelis-eval retrieval run \
    --golden eval/data/multihop-rag/strat500-golden.jsonl \
    --manifest eval/data/multihop-rag/strat500-manifest.jsonl \
    --config bm25 --config dense --config bm25+dense --concurrency 4
  ```

- **Model calls:** the embedding role went through `trelis-eval proxy --mode
  record` (#41). The `dense` config ran first and recorded each query's
  embedding at the provider. `bm25+dense` then replayed them, so its
  `dense/embed_query` step is about 2 ms.
- **Latency:** with the cost of one embedding call removed, the hybrid row's
  latency is mostly the dense search (p50 230 ms) plus BM25 (p50 13 ms); the
  two run concurrently. The re-run dense row's latency is the hosted
  embedding call (p50 1.9 s); its search step is p50 209 ms, against 21 s in
  the dense baseline before #143. Compare latency only between runs that
  made, or replayed, the same model calls.
