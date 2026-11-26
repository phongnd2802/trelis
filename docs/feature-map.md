# Feature map

What Trelis can do today, where each piece lives, and what is planned. Read this before exploring the code. Terms are the ones in `GLOSSARY.md`; decisions are in `docs/adr/`. Paths in backticks are checked by `scripts/check-feature-map.sh` (run by `make lint`), so a renamed or deleted file fails the build until this map is updated.

Status: **done** (built, tested, reachable), **partial** (code exists but nothing in the running service calls it yet), **planned** (only an issue).

## System at a glance

- **One Go process**, `cmd/server`, serves the HTTP API and runs the admin CLI (`server admin ...`). Background work will run as Temporal workflows (ADR-0005); no worker exists yet.
- **Stores** (`compose.yaml`, `compose.dev.yaml`):
  - Postgres (ParadeDB image, `pgvector` and `pg_search`): the source of truth (ADR-0002). Schema in `migrations/`, applied by goose on every start.
  - S3-compatible object storage (silo in dev): Source Files, later Figures (ADR-0006).
  - Neo4j: a rebuildable graph projection (ADR-0002). Only probed by `/readyz` today.
  - Temporal: background work (ADR-0005). Only probed by `/readyz` today.
- **Layers** (ADR-0007): `internal/router` (chi, middleware) → `internal/handler` (HTTP ↔ service) → `internal/application/service` (use cases, transactions) → `internal/application/repository` (Postgres through `db.Store`) → `internal/db` (sqlc queries, `ExecTx`). Contracts between layers are in `internal/types/interfaces`; domain types in `internal/types`.
- **A request's path**: `requestID` → `accessLog` → `recoverer` (`internal/router/logging.go`) → `authenticate` (`internal/router/authn.go`, puts the Principal in the context) → handler → service (checks the Access List) → repository.

## Documents and access

| Feature | Status | Where | Entry points | Tests | Decisions / issues |
|---|---|---|---|---|---|
| Principals, Groups, membership | done | `internal/application/service/principal.go`, `internal/application/repository/principal.go`, `migrations/00002_principals.sql` | `server admin principal create`, `group create`, `group add-member`; `GET /me` | `internal/application/service/principal_test.go`, `internal/application/repository/principal_test.go`, `internal/router/me_test.go` | #14, #110 |
| API keys (issue, list, revoke; stored hashed) | done | `internal/application/service/auth.go`, `internal/application/repository/apikey.go`, `internal/utils/apikey.go`, `migrations/00003_api_key_revocation.sql` | `server admin key create/list/revoke`; `Authorization: Bearer <key>` | `internal/utils/apikey_test.go`, `internal/application/repository/apikey_test.go`, `internal/router/revoke_test.go`, `cmd/server/admin_test.go` | #14, #99 |
| Authentication middleware (every route but the probes and Swagger needs a key, unknown paths included) | done | `internal/router/authn.go`, `internal/router/router.go` | — | `internal/router/router_test.go` | #14 |
| Upload a Document with an Access List | done | `internal/handler/document.go`, `internal/application/service/document.go`, `internal/application/repository/document.go`, `migrations/00004_documents.sql` | `POST /documents` (multipart: `file`, `principals`, `groups`); `storage.max_file_size_mb` | `internal/router/documents_test.go`, `internal/integration/documents_test.go` | ADR-0004, #20 |
| Read Documents (get, list; 404 for anything the caller cannot read) | done | same as upload; the caller comes from `internal/application/service/caller.go` | `GET /documents`, `GET /documents/{id}` | `internal/router/documents_test.go` | ADR-0004 |
| Duplicate upload detection (same caller, same content → 409 with the earlier Document) | done | `internal/application/service/document.go` | `POST /documents` | `internal/router/documents_test.go` | #20 |
| Source Files in object storage, downloaded through Trelis | done | `internal/application/service/file/`, `migrations/00005_object_storage.sql` | `GET /documents/{id}/file`; `storage.s3.*` | `internal/router/download_test.go` | ADR-0006, #105 |
| Figures (images taken out of a Document) | planned | — | — | — | #106 |
| Access control end to end (every retrieval path filtered by the Access List, Leakage tests) | planned | — | — | — | Week 5 (#6), #82 |

## Ingest

| Feature | Status | Where | Entry points | Tests | Decisions / issues |
|---|---|---|---|---|---|
| Parse office documents to Markdown (anydoc, in process, cgo) | partial | `internal/parser/anydoc_parser.go`, `internal/parser/anydoc/`, `third_party/anydoc-go/` | — (no caller yet); build with `make anydoc-lib`, `make build` | `internal/parser/anydoc/anydoc_test.go`, `internal/parser/anydoc/convert_linked_test.go` (`make test-linked`) | — |
| Supported upload types | done | `internal/utils/import_filetypes.go` | `POST /documents` rejects other types | `internal/router/documents_test.go` | — |
| Chunker: recursive tier, token limit, protected spans, validator | partial | `internal/chunker/` | — (no caller yet) | `internal/chunker/splitter_test.go`, `internal/chunker/strategy_test.go`, `internal/chunker/config_test.go` | #27 |
| Chunker: heading tier, heuristic tier (Vietnamese legal), tables, parent-child | planned | — | — | — | #28, #29, #30 |
| Ingest workflow on Temporal (parse, chunk, embed), reconciler, cancel on delete, progress over SSE | planned | — | — | — | Week 1 (#2): #21, #22, #26, #24, #23, #31 |
| OCR fallback (Docling, VLM), image captions | planned | — | — | — | Week 7 (#8): #76, #77, #78, #79 |

## Model clients

Each role (`models.chat`, `models.extraction`, `models.judge`, `models.embedding`, `models.rerank`) points at its own OpenAI-compatible provider; config in `internal/config/models.go`, documented in `config/trelis.yaml`. Every call logs one `model call` line (`internal/logger/modelcall.go`). None of the clients is called by the running service yet, so they are all **partial**.

| Feature | Status | Where | Tests | Decisions / issues |
|---|---|---|---|---|
| Chat role builder (picks the protocol, wraps the model-call log) | partial | `internal/models/chat/chat.go` | `internal/models/chat/chat_test.go` | #17, #19 |
| Chat Completions protocol (streaming, tool calls) | partial | `internal/models/api/openaicompletions/` | `internal/models/api/openaicompletions/client_test.go` | #17 |
| Responses protocol | partial | `internal/models/api/openairesponses/` | `internal/models/api/openairesponses/client_test.go`, `internal/models/api/openairesponses/golden_test.go` | #19 |
| Shared wire code: retry with backoff and `Retry-After`, SSE reader, stream assembler, capped reads | partial | `internal/models/api/` | `internal/models/api/retry_test.go`, `internal/models/api/sse_test.go`, `internal/models/api/stream_assembler_test.go` | — |
| Embeddings client (batching, dimension checks) | partial | `internal/models/embedding/` | `internal/models/embedding/embedding_test.go` | #16 |
| Rerank client (Jina/Cohere schema) | partial | `internal/models/rerank/` | `internal/models/rerank/rerank_test.go` | #18 |
| Fake OpenAI-compatible provider for tests | done | `internal/models/modeltest/` | its own tests | #15 |
| Agent engine | partial (stub) | `internal/agent/engine.go` | — | Week 5 (#6): #66 |

## HTTP API and developer surface

| Feature | Status | Where | Entry points | Tests | Decisions / issues |
|---|---|---|---|---|---|
| Liveness and readiness probes (Postgres, storage, Neo4j, Temporal) | done | `internal/router/router.go`, `internal/router/ready.go` | `GET /healthz`, `GET /readyz` | `internal/router/ready_test.go` | #12, #13 |
| Graceful shutdown | done | `internal/router/serve.go` | SIGINT/SIGTERM; `server.shutdown_timeout` | `internal/router/serve_test.go` | #12 |
| Response envelope and application errors | done | `internal/handler/response.go`, `internal/errors/` | — | `internal/handler/document_test.go` | — |
| Swagger UI and generated spec | done | `internal/apidocs/` (generated), swag annotations on handlers | `/swagger/index.html`, `/swagger/doc.json`; `server.swagger`; `make swagger` | `internal/router/swagger_test.go` | #117 |
| Request ID, access log, panic recoverer | done | `internal/router/logging.go` | `X-Request-Id` response header | `internal/router/logging_test.go` | ADR-0008, #119 |

## Platform

| Feature | Status | Where | Entry points | Tests | Decisions / issues |
|---|---|---|---|---|---|
| Config from YAML plus `TRELIS_*` env overrides, validated at load | done | `internal/config/` | `config/trelis.yaml`; `-config` flag | `internal/config/config_test.go` | #12 |
| Logging: zap, `env`-based defaults, stdout plus rotated JSON file, `logger.Ctx(ctx)` | done | `internal/logger/`, `internal/config/logging.go` | `env`, `log.*`; `TRELIS_ENV` | `internal/logger/logger_test.go`, `internal/config/config_test.go` | ADR-0008, #119 |
| Postgres connection, migrations, `db.Store` with context-carried transactions | done | `internal/db/`, `internal/db/queries/`, `internal/db/sqlc/` (generated), `migrations/` | `make sqlc` | `internal/db/store_test.go`, `internal/db/migrate_test.go` | ADR-0007, #13, #109 |
| Local stack | done | `compose.dev.yaml` (dependencies), `compose.yaml` (full stack), `scripts/stack-up.sh`, `deploy/temporal/` | `make dev-up`, `make dev`, `make stack-up`, `make dev-down` | CI `compose-smoke` job | #13, #97 |
| CI: lint (golangci-lint, depguard, gofmt, NOTICE, this map), vet, unit, linked, integration, compose smoke, Swagger spec freshness | done | `.github/workflows/ci.yml`, `.golangci.yml`, `scripts/check-notice.sh`, `scripts/check-feature-map.sh` | `make lint vet test`, `scripts/ci-wait.sh <branch>` | — | #11 |
| Integration tests: Trelis binary against Postgres, Neo4j and S3 in testcontainers, fake model provider | done | `internal/integration/` | `make test-integration` | `internal/integration/documents_test.go`, `internal/integration/me_test.go` | #15 |
| Tracing (OpenTelemetry to Langfuse) | planned | — | — | — | #74 |

## Planned areas

One line per roadmap milestone (#1); the issues hold the detail.

- **Retrieval** (hybrid BM25 + vector, rerank, query techniques): Week 2 (#3), Week 3 (#4).
- **Pipeline Answer** with citations, generation graders: Week 3 (#4), #42.
- **GraphRAG**: Extractions, cross-document Entity Resolution, Neo4j projection, graph read API: Week 4 (#5), ADR-0003, #56, #60, #61.
- **Agent Answer, tools, MCP server**: Week 5 (#6), #66, #67, #69, #70.
- **Wiki** (Summary, Entity and Index Pages), **MCP client** with Tool Approval: Week 6 (#7), #62–#65, #71–#73.
- **Eval harness** (external Python, Golden Set, Ablation Matrix): ADR-0001, Week 2 (#3), #38, #81, #88.
- **Frontend, demo**: Week 8 (#9), #83, #84, #86, #87, #89.
