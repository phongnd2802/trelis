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

## Tests

- Behaviour is tested at the highest seam that covers it: the router with `httptest` for HTTP behaviour, `internal/integration` when real Postgres, S3 or Neo4j matter, and unit tests for pure logic (chunker, config, retry).
- Code that calls a model is tested against `internal/models/modeltest`'s fake provider, never a real one.
- A test that checks logging asserts on fields through `zaptest/observer`, and asserts that no content (prompts, inputs, Document text) appears.
