# Ablation Matrix

Built from 4 Ablation Run results files alone: the same files and seed give this report byte for byte.

- Golden Set: `eval/data/multihop-rag/strat500-golden.jsonl`
- Golden Set sha256: `01496451025b5d434d5a3a8027505e8986d5d3441012bdc232f80e3f60ead07e`
- Golden Set questions: 500
- top_k: 10
- Bootstrap: 95% confidence, 10000 resamples, seed 1

Confidence intervals are percentile bootstrap intervals over the scored questions.

| Retrieval Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Latency p50 (ms) | Latency p95 (ms) | Scored | Skipped | Error | Concurrency |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| bm25+dense+rerank | 0.736 [0.711, 0.762] | 0.840 [0.820, 0.860] | 0.895 [0.872, 0.916] | 0.774 [0.754, 0.793] | 1221.3 [1217.5, 1226.7] | 1409.9 [1384.2, 1518.7] | 441 | 59 | 0 | 4 |
| bm25+dense | 0.666 [0.637, 0.694] | 0.811 [0.788, 0.833] | 0.828 [0.800, 0.856] | 0.714 [0.692, 0.736] | 243.5 [242.3, 244.9] | 259.0 [254.6, 262.3] | 441 | 59 | 0 | 4 |
| bm25 | 0.621 [0.590, 0.650] | 0.744 [0.717, 0.770] | 0.780 [0.748, 0.811] | 0.658 [0.633, 0.684] | 21.0 [20.7, 21.3] | 29.1 [28.3, 30.0] | 441 | 59 | 0 | 4 |
| dense | 0.630 [0.603, 0.656] | 0.779 [0.755, 0.801] | 0.797 [0.768, 0.826] | 0.678 [0.657, 0.700] | 23925.0 [23474.7, 24389.3] | 34511.5 [32214.2, 35334.9] | 441 | 59 | 0 | 4 |

## Runs

| Retrieval Config | Run ID | Harness version |
| --- | --- | --- |
| bm25+dense+rerank | strat500-rerank-20261128b | 0.1.0 |
| bm25+dense | strat500-hybrid-20261127 | 0.1.0 |
| bm25 | strat500-hybrid-20261127 | 0.1.0 |
| dense | baseline-dense-strat500-seed1 | 0.1.0 |
