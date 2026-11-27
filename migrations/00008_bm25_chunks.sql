-- BM25 retrieval indexes the text dense embeds, ContextHeader + "\n\n" +
-- Content, at Vietnamese syllable level: Vietnamese writes one syllable per
-- space-separated token, so unicode-word tokens are syllables. The folded
-- field repeats that text with ASCII folding, so a query typed without
-- diacritics ("thue tncn") still matches "thuế TNCN".
--
-- document_id is indexed so the Access List filter is pushed into the BM25
-- search before its top-K, not applied after a global top-K.
-- +goose Up
CREATE INDEX chunks_bm25_idx ON chunks
USING bm25 (
    id,
    ((context_header || E'\n\n' || content)::pdb.unicode_words('alias=chunk_text')),
    ((context_header || E'\n\n' || content)::pdb.unicode_words('ascii_folding=true', 'alias=chunk_text_folded')),
    document_id
);

-- +goose Down
DROP INDEX chunks_bm25_idx;
