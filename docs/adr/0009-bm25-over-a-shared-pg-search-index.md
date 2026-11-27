# BM25 uses one shared pg_search index, with syllable-level and ASCII-folded fields

BM25 (`retrieval_config.techniques: ["bm25"]`, #33) ranks Chunks with pg_search over one `bm25` index on `chunks`. The index stores the same text dense embeds (`ContextHeader + "\n\n" + Content`, `chunkText` in `internal/application/service/ingest.go`) twice. `chunk_text` uses `pdb.unicode_words`: Vietnamese writes one syllable per space-separated token, so unicode-word tokens are syllables. The tokenizer lowercases by default and uses no stemming or stopwords, which keeps the index close to the legal Vietnamese text and avoids language-specific guesses. `chunk_text_folded` repeats the text with `ascii_folding=true`, so "thue tncn" finds "thuế TNCN" and `đ` folds to `d`. A query is a `|||` match disjunction over both fields: pg_search tokenizes the query with each field's tokenizer, so a query with diacritics matches the exact field and, through folding, the folded one too. `|||` is not the Tantivy query-string parser, so `:`, `(`, `"`, `AND`, `-`, `*` are only separators and can neither error nor change the query's meaning. `document_id` is indexed, so `document_id = ANY(...)` is pushed into the Tantivy query and applied before its top-K; a Chunk of a Restricted Document is never returned and never takes a readable Chunk's place (ADR-0004).

A query with diacritics matches both fields, and pg_search adds their scores; a query typed without diacritics matches only the folded field. That scoring asymmetry is deliberate: the exact field adds evidence when the caller typed the diacritics, while the folded field keeps the no-diacritics query working. Raw BM25 scores are not comparable across queries — repeating a query word inflates the score — so fusion (#34) must combine ranks, not raw scores.

The index is shared by every Document, so BM25's IDF and average Chunk length come from the whole corpus, Restricted Documents included. A Restricted Document is never returned, but its terms and length shift the scores and ranking of readable Chunks, and `/retrieve` returns those raw scores, so in principle a caller could probe whether a term occurs in Documents they cannot read. We accept this as standard practice for a shared BM25 index; #55's Leakage Grader can measure it.

## Considered Options

- **Return only ranks, not raw BM25 scores.** Rejected: hiding the number does not remove the influence, because the order still comes from the shared corpus statistics, and both the API contract and the eval harness use the per-technique score.
- **Compute statistics per caller.** Rejected: IDF and average length would have to be recomputed over the caller's readable set on every request, or one index per Access List combination would have to be maintained; both grow with Documents and Groups and leave scores incomparable between callers.

## Consequences

- BM25 needs no embedding model, so a bm25-only request answers when `models.embedding` is unconfigured; the 503 stays for dense.
- The text expression appears in the index, in `SearchBM25Chunks` and in `chunkText`. They must stay identical: pg_search only uses the index when the query expression matches it exactly, and the index must cover the text dense embeds.
- The index keeps two tokenized copies of each Chunk, so every insert and delete maintains both.
- The readable set is still resolved per request and passed into the query; the Access List filter stays inside the BM25 query, before top-K.
- A Restricted Document can shift a readable Chunk's score and ranking; the corpus-statistics side channel is accepted and measured by #55, not eliminated.
- Fusion (#34) uses ranks rather than raw scores, because raw BM25 scores are not comparable across queries.