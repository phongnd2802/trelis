# Entities are resolved across Documents; Omnisc's per-document graph is kept as a baseline

Every Mention is resolved to one Entity shared by all Documents. Resolution runs in this order: normalized name (including legal document numbers), alias match, embedding similarity above a threshold, then LLM confirmation for ambiguous pairs. Graph seeds are found through a vector index on Entities plus name/alias match, and Chunks reached through the graph are fused with other Retrieval Techniques by RRF. We chose this because the Benchmark Corpus (MultiHop-RAG) asks cross-document questions, and a graph whose nodes stop at a Document's edge cannot make the second hop.

## How this differs from Omnisc

Omnisc keys each Neo4j node by `{name, kg: knowledge_id}` (`neo4j/repository.go`), so "Thông tư 111" mentioned in two Documents becomes two unconnected nodes, and Relations exist only inside one Document. It finds seeds by substring `CONTAINS` on names, so "TT 111" does not match "Thông tư 111/2013/TT-BTC", and it appends graph Chunks to the end of the search results without ranking them. Its Wiki does resolve entities across Documents, through slugs and dedup, so Omnisc keeps two separate entity models. Trelis keeps one, and its Wiki Pages are written from these Entities.

## Consequences

- Because Extractions live in Postgres (ADR-0002), both the `per-document` graph (Omnisc's behaviour) and the `resolved` graph are projected from the same Extractions. They appear as two rows of the Ablation Matrix.
- Over-merging, such as treating Thông tư 111/2013 and 111/2014 as one Entity, creates false hops. Entity Resolution therefore gets its own precision/recall check on about 100 hand-labelled pairs.
