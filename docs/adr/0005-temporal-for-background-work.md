# Temporal runs background work; agent turns stay in the API process

Ingest (parse, OCR fallback, chunk, embed, per-Chunk extraction fan-out, Entity Resolution, Neo4j projection), Wiki rebuilds and graph rebuilds run as Temporal workflows. LLM-calling activities run on a dedicated task queue whose `TaskQueueActivitiesPerSecond` is the global LLM rate limit. Temporal's persistence and visibility use a separate database on the same Postgres instance. We chose Temporal because the ingest path is long and branching: per-activity retries, fan-out and wait, cancellation when a Document is deleted mid-ingest, and a UI showing where a Document is stuck come built in.

## Considered Options

- **asynq on Redis** (Omnisc). Rejected: it adds Redis, and because enqueueing is not part of the Postgres transaction it causes the "stuck processing" bugs Omnisc works around with pending-subtask counters and stuck-knowledge sweeps.
- **River on Postgres.** A strong runner-up, since it enqueues inside the transaction. Rejected because multi-step pipelines, fan-out and global rate limits (a paid tier in River) would have to be hand-built.
- **Hand-rolled `SKIP LOCKED` queue.** Rejected: one to two weeks spent rebuilding retries, leases and cancellation.

## Consequences

- Starting a workflow is not part of the Postgres transaction. The workflow ID is the Document ID, so a start can be retried safely, and a reconciler restarts the workflow for any Document left pending without one.
- Workflow code must be deterministic. All I/O happens in activities, and changing a running workflow's logic needs versioning.
- Workflows pass IDs, never Document or Chunk content (payload size limits). Large Documents fan out through child workflows in batches to stay within history limits.
- Agent turns are not workflows, because tokens must stream over SSE with low latency. Tool Approvals are stored in Postgres. Running the agent on Temporal is a tier-3 option.
