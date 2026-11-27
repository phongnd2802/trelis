package types

import (
	"time"

	"github.com/google/uuid"
)

// Chunk is a contiguous piece of a Document's text that is retrieved and
// cited on its own (GLOSSARY.md). Content is the source text between
// StartOffset and EndOffset, which are rune offsets into the Document's
// Markdown, so EndOffset-StartOffset is Content's length in runes.
type Chunk struct {
	// Unique identifier of the Chunk
	ID uuid.UUID `json:"id"`
	// Document this Chunk belongs to
	DocumentID uuid.UUID `json:"document_id"`
	// Seq orders the Chunks of one Document, starting at 0
	Seq int `json:"seq"`
	// Content is the source text of the Chunk
	Content string `json:"content"`
	// ContextHeader is context that is not in the source at the offsets,
	// such as a heading breadcrumb or a repeated table header
	ContextHeader string `json:"context_header"`
	// StartOffset is the first rune of the Document's Markdown in Content
	StartOffset int `json:"start_offset"`
	// EndOffset is the rune just past the last one in Content
	EndOffset int `json:"end_offset"`
	// Creation time of the Chunk
	CreatedAt time.Time `json:"created_at"`
}

// ParentChunk is a wider span of a Document's text around consecutive
// Chunks. It gives an answer more context and is never embedded or searched
// itself, so it lives in its own table (GLOSSARY.md). Content is the source
// text between StartOffset and EndOffset, which are rune offsets into the
// Document's Markdown like Chunk's.
type ParentChunk struct {
	// Unique identifier of the Parent Chunk
	ID uuid.UUID `json:"id"`
	// Document this Parent Chunk belongs to
	DocumentID uuid.UUID `json:"document_id"`
	// Seq orders the Parent Chunks of one Document, starting at 0
	Seq int `json:"seq"`
	// Content is the source text the Parent Chunk covers
	Content string `json:"content"`
	// ContextHeader is context that is not in the source at the offsets,
	// copied from the first of the Parent Chunk's Chunks
	ContextHeader string `json:"context_header"`
	// StartOffset is the first rune of the Document's Markdown in Content
	StartOffset int `json:"start_offset"`
	// EndOffset is the rune just past the last one in Content
	EndOffset int `json:"end_offset"`
	// Creation time of the Parent Chunk
	CreatedAt time.Time `json:"created_at"`
}

// ChunkSet is the whole Chunk set of one Document to store: the Parent Chunks
// and the Chunks, with ParentOf[i] the index in Parents of Chunks[i]'s Parent
// Chunk. ParentOf is nil when the Document has no Parent Chunks, and Parents
// is then empty too.
type ChunkSet struct {
	Parents  []*ParentChunk
	Chunks   []*Chunk
	ParentOf []int
}

// ChunkingResult reports what one parse-and-chunk step stored and how the
// chunker chose it. Reasons mention sizes and offsets, never Document text.
type ChunkingResult struct {
	// Chunks is how many embedded Chunks were stored.
	Chunks int
	// ParentChunks is how many Parent Chunks were stored; 0 when parent-child
	// mode was off.
	ParentChunks int
	// Tier is the tier whose output was used.
	Tier string
	// TierChain is every tier tried, in order.
	TierChain []string
	// Rejected holds the tiers whose output the validator rejected, with why.
	Rejected []ChunkingRejection
}

// ChunkingRejection is one rejected chunking tier and the validator's reason.
type ChunkingRejection struct {
	Tier   string
	Reason string
}
