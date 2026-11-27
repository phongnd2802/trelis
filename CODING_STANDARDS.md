# Coding standards

The rules a reviewer applies to a diff. Every rule here is a judgement call; what tooling already enforces (gofmt, golangci-lint with depguard and forbidigo, the NOTICE and feature-map checks, the Swagger drift test, `make swagger` freshness in CI) is not repeated. Cite the rule's heading in a finding.

## Access and Leakage

Every read of a Document, or of anything derived from one (Chunks, Figures, Extractions, Entities, Relations, Wiki Pages, answers), is filtered by the caller's Access List before it leaves the service layer (ADR-0004).

- A Principal who cannot read a Document gets the same answer as for one that does not exist: the same status and body. Counts, totals, error messages and citations reveal nothing about Documents the caller cannot read.
- Derived data follows ADR-0004 exactly: a graph result needs at least one readable Mention or evidence Chunk; a Wiki Page needs every source Document readable.
- A new read path comes with a test where a second Principal, not on the Access List, gets nothing (as in `internal/router/documents_test.go`).

## Layers

ADR-0007: handlers call services, services call repositories, repositories reach Postgres only through `db.Store`.

- A handler parses and renders; access decisions and transactions live in the service.
- Only a service starts a transaction (`store.ExecTx`); a repository never does.
- A new contract between layers is declared in `internal/types/interfaces`.

## Logging

Log through `logger.Ctx(ctx)` (ADR-0008).

- **Once, at the boundary.** An error is logged where it stops travelling: the handler or middleware for HTTP, the activity for Temporal. Inner layers wrap it with `%w` and return it. The one exception is an error the code swallows (a failed cleanup, a fallback): log it where it is swallowed.
- **IDs, not content.** A record carries IDs (Document, Principal, API key ID, request) and metadata (sizes, counts, statuses, durations). It never carries secrets, the `Authorization` header, request or response bodies (a vendor's error body included), Document or Chunk text, prompts or completions, or Document file names: whoever reads logs is not on a Restricted Document's Access List.
- **Routes, not paths.** HTTP records name the chi route pattern (`/documents/{id}`), never the raw path or query string.
- **Levels.** ERROR is something an operator must look at; WARN is a degraded but handled case; INFO is one line per meaningful event (a request, a model call, a start or stop); DEBUG is for tracing a flow. A loop over many items logs a summary, not a line per item, unless it is DEBUG.
- **Stable records.** A message or field that CI or a test reads (see the comments in `cmd/server/main.go`) changes only together with its reader.

## HTTP API

- A new or changed route carries swag annotations whose responses match what the handler actually writes: every status it can return, with the right body type (handler envelope or the router's `{"error"}`).
- Errors reach the client as an `AppError` with a stable code; any other error becomes a generic 500 and its cause goes to the log, never to the response.

## Domain language and docs

- Names, comments, log messages and test names use the terms in `GLOSSARY.md`, not the synonyms it lists under _Avoid_.
- Every exported identifier has a doc comment that says what it is for; comments explain why, not what the next line does.
- A change that adds, moves or changes the status of a feature updates its row in `docs/feature-map.md`.
- A decision that is hard to reverse, surprising without context, and the result of a real trade-off gets an ADR in `docs/adr/`.
- Code copied near-verbatim from WeKnora or Omnisc keeps the Tencent copyright header and a NOTICE entry; code rewritten from their ideas carries neither.

## Temporal

ADR-0005: background work runs as workflows that pass IDs and do their I/O in activities.

- **One model request per activity.** A model-calling activity sends one request (one embeddings batch, one extraction, one caption) and runs on `temporal.model_task_queue`, so `TaskQueueActivitiesPerSecond` limits model calls rather than activity starts, and a retry repeats one request. A Document's work fans out as one activity per batch or per Chunk.
- **Idempotent over the input.** An activity that is retried, or runs again after a crash, skips what an earlier attempt already committed (for example, it selects only the Chunks with no embedding yet) instead of redoing the whole Document.
- **Status follows the data.** A status that claims derived data is complete (`completed`, ready) is set in the same transaction that checks it: no Chunk without its embedding, no Chunk without its Extractions. An update that hits no row is an error, never a silent skip.
- **Versioned changes.** A change to the commands a registered workflow schedules carries `workflow.GetVersion`, or a comment saying why no run in flight can replay against it.

## Tests

- Behaviour is tested at the highest seam that covers it: the router with `httptest` for HTTP behaviour, `internal/integration` when real Postgres, S3 or Neo4j matter, and unit tests for pure logic (chunker, config, retry).
- Code that calls a model is tested against `internal/models/modeltest`'s fake provider, never a real one.
- A test that checks logging asserts on fields through `zaptest/observer`, and asserts that no content (prompts, inputs, Document text) appears.
- A regression test is shown **red**: without the fix it fails on its own assertion, not on a build error, an SQL error or a skip. `scripts/red-check.sh <package> <test-regexp> <fix files>...` puts the fix files back as they are on `main`, runs the test and restores them; `--each` does the same for several Go tests and `--py <node-id>,...` for pytest tests in `eval/`, passing only when every test is red. The PR quotes the failing line. A control inside the test (a copy of the old query, a forced plan) asserts that its setup still reproduces the bug, so the test cannot pass by accident.
