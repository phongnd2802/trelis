# Ablation Matrix

Built from 4 Ablation Run results files and the price table alone: the same files, seed and price table give this report byte for byte.

- Golden Set: `eval/data/multihop-rag/strat500-golden.jsonl`
- Golden Set sha256: `01496451025b5d434d5a3a8027505e8986d5d3441012bdc232f80e3f60ead07e`
- Golden Set questions: 500
- top_k: 10
- Bootstrap: 95% confidence, 50 resamples, seed 1

Confidence intervals are percentile bootstrap intervals over the observations each estimate uses; a Grader's interval covers its ok grades and excludes grader_error and not_applicable.

| Retrieval Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Latency p50 (ms) | Latency p95 (ms) | Tokens | Cost per Correct Answer (USD) | Scored | Skipped | Error | Concurrency |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| bm25+dense+rerank | 0.736 [0.710, 0.757] | 0.840 [0.820, 0.863] | 0.895 [0.872, 0.927] | 0.774 [0.748, 0.795] | unknown | unknown | — | — | 441 | 59 | 0 | 4 |
| bm25+dense | 0.666 [0.640, 0.690] | 0.811 [0.786, 0.828] | 0.828 [0.802, 0.859] | 0.714 [0.688, 0.735] | unknown | unknown | — | — | 441 | 59 | 0 | 4 |
| bm25 | 0.621 [0.595, 0.660] | 0.744 [0.718, 0.766] | 0.780 [0.759, 0.827] | 0.658 [0.635, 0.680] | unknown | unknown | — | — | 441 | 59 | 0 | 4 |
| dense | 0.630 [0.599, 0.651] | 0.779 [0.759, 0.796] | 0.797 [0.768, 0.820] | 0.678 [0.661, 0.701] | unknown | unknown | — | — | 441 | 59 | 0 | 4 |

Latency is reported only for live and passthrough runs: a record run mixes recorded upstream calls with replayed ones, so its latency measures neither the provider nor the replay; a replay run replays every call, and a run from before model modes is unknown, so those cells show replayed or unknown.

## Runs

| Retrieval Config | Run ID | Harness version |
| --- | --- | --- |
| bm25+dense+rerank | strat500-rerank-20261128b | 0.1.0 |
| bm25+dense | strat500-hybrid-20261127 | 0.1.0 |
| bm25 | strat500-hybrid-20261127 | 0.1.0 |
| dense | baseline-dense-strat500-seed1 | 0.1.0 |

## Slices

### comparison_query

167 questions

| Retrieval Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Scored |
| --- | --- | --- | --- | --- | --- |
| bm25+dense+rerank | 0.802 [0.765, 0.837] | 0.885 [0.862, 0.913] | 0.918 [0.889, 0.953] | 0.814 [0.789, 0.839] | 167 |
| bm25+dense | 0.708 [0.662, 0.748] | 0.858 [0.829, 0.882] | 0.832 [0.796, 0.867] | 0.742 [0.719, 0.780] | 167 |
| bm25 | 0.686 [0.648, 0.731] | 0.816 [0.769, 0.852] | 0.786 [0.739, 0.838] | 0.699 [0.660, 0.742] | 167 |
| dense | 0.650 [0.603, 0.706] | 0.793 [0.738, 0.830] | 0.771 [0.718, 0.813] | 0.677 [0.645, 0.710] | 167 |

### inference_query

160 questions

| Retrieval Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Scored |
| --- | --- | --- | --- | --- | --- |
| bm25+dense+rerank | 0.628 [0.581, 0.689] | 0.777 [0.754, 0.811] | 0.865 [0.820, 0.891] | 0.711 [0.680, 0.740] | 160 |
| bm25+dense | 0.592 [0.554, 0.629] | 0.758 [0.729, 0.784] | 0.836 [0.798, 0.898] | 0.677 [0.643, 0.723] | 160 |
| bm25 | 0.515 [0.464, 0.556] | 0.647 [0.610, 0.683] | 0.747 [0.702, 0.792] | 0.583 [0.535, 0.623] | 160 |
| dense | 0.580 [0.551, 0.621] | 0.742 [0.703, 0.780] | 0.790 [0.731, 0.834] | 0.649 [0.614, 0.685] | 160 |

### null_query

59 questions

| Retrieval Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Scored |
| --- | --- | --- | --- | --- | --- |
| bm25+dense+rerank | n/a | n/a | n/a | n/a | 0 |
| bm25+dense | n/a | n/a | n/a | n/a | 0 |
| bm25 | n/a | n/a | n/a | n/a | 0 |
| dense | n/a | n/a | n/a | n/a | 0 |

### temporal_query

114 questions

| Retrieval Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Scored |
| --- | --- | --- | --- | --- | --- |
| bm25+dense+rerank | 0.791 [0.753, 0.827] | 0.863 [0.829, 0.895] | 0.904 [0.849, 0.939] | 0.806 [0.767, 0.841] | 114 |
| bm25+dense | 0.709 [0.618, 0.776] | 0.814 [0.779, 0.855] | 0.812 [0.761, 0.863] | 0.726 [0.675, 0.769] | 114 |
| bm25 | 0.674 [0.629, 0.732] | 0.772 [0.719, 0.823] | 0.818 [0.762, 0.877] | 0.705 [0.658, 0.743] | 114 |
| dense | 0.670 [0.605, 0.737] | 0.808 [0.762, 0.849] | 0.846 [0.802, 0.905] | 0.722 [0.683, 0.760] | 114 |

### hops-0

59 questions

| Retrieval Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Scored |
| --- | --- | --- | --- | --- | --- |
| bm25+dense+rerank | n/a | n/a | n/a | n/a | 0 |
| bm25+dense | n/a | n/a | n/a | n/a | 0 |
| bm25 | n/a | n/a | n/a | n/a | 0 |
| dense | n/a | n/a | n/a | n/a | 0 |

### hops-2

231 questions

| Retrieval Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Scored |
| --- | --- | --- | --- | --- | --- |
| bm25+dense+rerank | 0.861 [0.835, 0.883] | 0.926 [0.900, 0.944] | 0.934 [0.902, 0.958] | 0.856 [0.837, 0.879] | 231 |
| bm25+dense | 0.792 [0.753, 0.825] | 0.898 [0.877, 0.918] | 0.870 [0.845, 0.910] | 0.795 [0.761, 0.815] | 231 |
| bm25 | 0.768 [0.727, 0.812] | 0.861 [0.838, 0.894] | 0.853 [0.821, 0.882] | 0.765 [0.739, 0.802] | 231 |
| dense | 0.716 [0.669, 0.751] | 0.846 [0.816, 0.877] | 0.822 [0.783, 0.859] | 0.732 [0.695, 0.760] | 231 |

### hops-3

149 questions

| Retrieval Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Scored |
| --- | --- | --- | --- | --- | --- |
| bm25+dense+rerank | 0.642 [0.611, 0.691] | 0.765 [0.738, 0.803] | 0.868 [0.841, 0.910] | 0.708 [0.670, 0.739] | 149 |
| bm25+dense | 0.564 [0.515, 0.602] | 0.738 [0.705, 0.790] | 0.771 [0.725, 0.819] | 0.637 [0.598, 0.672] | 149 |
| bm25 | 0.488 [0.452, 0.530] | 0.642 [0.586, 0.685] | 0.703 [0.643, 0.759] | 0.562 [0.525, 0.596] | 149 |
| dense | 0.555 [0.528, 0.591] | 0.714 [0.664, 0.749] | 0.759 [0.714, 0.811] | 0.624 [0.593, 0.661] | 149 |

### hops-4

61 questions

| Retrieval Config | Recall@5 | Recall@10 | MRR | nDCG@10 | Scored |
| --- | --- | --- | --- | --- | --- |
| bm25+dense+rerank | 0.492 [0.422, 0.537] | 0.697 [0.648, 0.746] | 0.811 [0.742, 0.898] | 0.628 [0.560, 0.672] | 61 |
| bm25+dense | 0.439 [0.410, 0.480] | 0.656 [0.611, 0.684] | 0.810 [0.723, 0.882] | 0.598 [0.541, 0.635] | 61 |
| bm25 | 0.385 [0.344, 0.439] | 0.545 [0.488, 0.586] | 0.693 [0.574, 0.790] | 0.492 [0.438, 0.538] | 61 |
| dense | 0.484 [0.447, 0.529] | 0.680 [0.631, 0.717] | 0.797 [0.728, 0.892] | 0.609 [0.577, 0.660] | 61 |

## Failure Attribution

No answer Ablation Run in this report, so there is nothing to attribute.
