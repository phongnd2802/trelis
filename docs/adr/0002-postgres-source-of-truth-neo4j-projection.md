# Postgres is the source of truth; Neo4j is a rebuildable projection

Every Extraction (the entities and relations an LLM read out of one Chunk) is stored in Postgres next to the Chunk it came from, and Access Lists live only in Postgres. Neo4j holds a projection of those Extractions after Entity Resolution, and a rebuild command can drop it and regenerate it from Postgres without calling an LLM. We chose this because the expensive, non-deterministic step (LLM extraction) should be paid for once. Entity Resolution can then be changed and re-run cheaply, the per-document baseline and the resolved graph can be projected from the same Extractions for the Ablation Matrix, and deleting a Document or changing its Access List has a single place where the answer is decided.

## Considered Options

- **Neo4j as the only store of graph data** (what Omnisc does: the LLM output goes straight into `apoc.merge.node`). Rejected: changing Entity Resolution or comparing graph variants would mean paying for extraction again, and access rules would be split across two databases.
- **Graph in Postgres only** (tables plus recursive CTEs). Rejected by choice: Neo4j's traversal and vector index on Entities are wanted, and the projection boundary keeps that choice reversible.

## Consequences

- Neo4j can lag behind Postgres. Retrieval treats graph results as candidate Chunk IDs and re-checks them against Postgres (existence and Access List) before using them.
- Writing an Extraction is an idempotent activity (an upsert keyed by Chunk ID) inside the ingest workflow (ADR-0005). Projecting into Neo4j is a separate, idempotent step.
