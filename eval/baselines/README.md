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

The 59 `null_query` questions are skipped, with 0 errors. These are point
estimates; confidence intervals come with #40.

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
