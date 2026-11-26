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
