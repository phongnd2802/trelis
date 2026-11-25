# The eval harness is an external Python program that talks to Trelis over HTTP

The service is written in Go, but the eval harness is a separate Python program that calls Trelis through its public HTTP API, using each Principal's API key, the same way any other client would. We chose this so that every Ablation Run measures the path real users take: auth, Access Lists, SSE and the real Retrieval Config wiring. The harness cannot reach into internals and skip steps, results become versioned files rather than server state, and the statistics we need (bootstrap confidence intervals, Cohen's kappa for judge calibration, slicing) are routine in Python.

## Considered Options

- **In-process Go evaluation** (what Omnisc does in `service/evaluation.go`: it creates a throwaway knowledge base, runs the internal chat pipeline, scores BLEU/ROUGE/MRR/NDCG through a metric hook and keeps results in an in-memory map that is lost on restart, one model configuration per run, with no judge and no Principal). Rejected: it bypasses the code paths under test, cannot measure Leakage, and cannot produce an Ablation Matrix.
- **All-Python service.** Rejected: the service side is meant to show Go backend work, and the vendored anydoc parser is Go/cgo.

## Consequences

- The API must accept a `retrieval_config` on each request; a single global configuration is not enough to run an Ablation Matrix from outside.
- The API must offer a retrieval-only endpoint, plus a debug flag on answer endpoints, that returns the retrieved Chunk IDs, scores and trace. Retrieval metrics are computed from these, and retrieval can be measured without paying for generation.
- Model calls happen inside the Go service, out of the harness's reach. The harness therefore runs an OpenAI-compatible **record/replay proxy** (chat completions, responses, embeddings, rerank), and for eval and CI, Trelis's per-role `base_url` points at it. The proxy keys each response by a hash of the normalized request body, stores streamed responses as their original chunk sequence, and keeps the original `usage` so cost stays real. It has three modes:
  - `record`: call the real provider and save the response;
  - `replay`: fixtures only, and a missing fixture is a hard failure;
  - `passthrough`: nothing saved, used for real latency measurement.

  CI runs in strict `replay` with fixtures committed to the repo and no provider keys. When a change alters prompts or retrieved Chunks, the request hash changes, replay reports the missing fixture, and the developer re-records locally. This is intended: a retrieval change must regenerate answers before its effect can be measured. All Go code still runs under test, and only the model is replaced. The service has no response cache and no knowledge of the proxy; to it the proxy is just another OpenAI-compatible provider (backend caching is deliberately deferred).
- Two languages and two dependency sets have to be maintained.
