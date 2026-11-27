// Package chunker splits a Document's text into Chunks.
//
// Split resolves a chain of tiers from SplitterConfig.Strategy, runs each
// tier in turn, and returns the first output that ValidateChunks accepts.
// StrategyAuto picks the heading tier when a Document's Markdown headings are
// dense enough and otherwise the heuristic tier when its plain-line sections
// are; both put the section breadcrumb their Content does not already hold in
// ContextHeader. The recursive tier always ends the chain and its output is
// returned even when rejected, so non-blank text always yields Chunks.
// Protected spans (fenced code, tables, math, inline code, links and images)
// are kept whole within a Chunk; overlap between Chunks starts at a semantic
// boundary.
package chunker

import (
	"strings"
	"unicode/utf8"
)

// Chunk is a contiguous piece of a Document's text.
//
// Content is exactly the source text between Start and End, which are rune
// offsets (so End-Start == utf8.RuneCountInString(Content)), the way
// Postgres counts characters. Context that is not in the source at those
// offsets, such as a heading breadcrumb or the header row of a table that
// continues from the previous Chunk, goes in ContextHeader so the offsets
// stay exact.
type Chunk struct {
	Content       string
	ContextHeader string
	Seq           int
	Start         int
	End           int
}

// EmbeddingContent is the text to embed: ContextHeader (when set), a blank
// line, then Content without its surrounding whitespace.
func (c Chunk) EmbeddingContent() string {
	body := strings.TrimSpace(c.Content)
	if c.ContextHeader == "" {
		return body
	}
	return c.ContextHeader + "\n\n" + body
}

// runeIndex converts byte offsets of one text into rune offsets. It is
// fastest when offsets are asked for in increasing order.
type runeIndex struct {
	text       string
	byteAt, at int
}

func (x *runeIndex) runeOffset(b int) int {
	if b < x.byteAt {
		x.byteAt, x.at = 0, 0
	}
	x.at += utf8.RuneCountInString(x.text[x.byteAt:b])
	x.byteAt = b
	return x.at
}
