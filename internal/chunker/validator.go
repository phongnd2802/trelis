package chunker

import (
	"fmt"
	"unicode"
)

// tinyChunkRunes is the size under which a Chunk, other than the last,
// counts as tiny.
const tinyChunkRunes = 50

// ValidationResult is ValidateChunks' verdict on one tier's output.
type ValidationResult struct {
	OK     bool
	Reason string
}

// ValidateChunks decides whether a tier's Chunks for text are usable, or
// whether the chain should fall through to the next tier. cfg must have its
// defaults applied. Each Chunk must be the source text at its offsets and
// must not start or end inside a protected span, unless the span is too
// large to keep whole (see SplitterConfig.TokenLimit); a table too large for
// one Chunk may also be cut at a row start, or anywhere inside a row that is
// itself larger than the room the Chunk has for Content once its
// ContextHeader is counted. Beyond that the check is deliberately lenient
// and catches only plainly broken output, with T the target size
// cfg.ChunkSize:
//
//   - no Chunks;
//   - one Chunk for text over 2T runes that is not one protected span:
//     the tier did not split;
//   - more than two Chunks, and over a quarter of them, under 50 runes
//     (the last Chunk is not counted): the tier fragmented the text;
//   - no Chunk reaching T/4 runes for text over T: same;
//   - a Chunk over 2T runes that is not a protected span: the tier ignored
//     its budget.
func ValidateChunks(text string, chunks []Chunk, cfg SplitterConfig) ValidationResult {
	reject := func(format string, args ...any) ValidationResult {
		return ValidationResult{Reason: fmt.Sprintf(format, args...)}
	}
	if len(chunks) == 0 {
		return reject("no chunks")
	}
	target := cfg.ChunkSize
	runes := []rune(text)
	protectedBytes := protectedSpans(text)
	protected := toRuneSpans(text, protectedBytes)
	if len(chunks) == 1 && len(runes) > 2*target && !withinSpan(protected, runes, chunks[0]) {
		return reject("one chunk for a document of %d runes", len(runes))
	}
	wholeLimit := protectedLimit(cfg, text)
	layouts := make([]tableLayout, len(protected))
	for i, s := range protectedBytes {
		if isTableSpan(text, s) && protected[i].end-protected[i].start > target {
			layouts[i] = tableLayoutIn(text, s, protected[i])
		}
	}
	breaksProtectedSpan := func(pos, chunkStart, budget int) bool {
		i := firstEndingAfter(protected, pos)
		if i >= len(protected) || protected[i].start >= pos {
			return false
		}
		if !isTableSpan(text, protectedBytes[i]) {
			return protected[i].end-protected[i].start <= wholeLimit
		}
		if protected[i].end-protected[i].start <= target {
			return true
		}
		return tableCutBreaksSpan(protected[i], layouts[i], pos, chunkStart, budget)
	}

	longest, tiny := 0, 0
	for i, c := range chunks {
		if c.Start < 0 || c.End > len(runes) || c.Start >= c.End || string(runes[c.Start:c.End]) != c.Content {
			return reject("chunk %d is not the source text at [%d,%d)", i, c.Start, c.End)
		}
		// A Chunk's Content budget is what its ContextHeader leaves of the
		// target; a row the splitter cut inside must be longer than that.
		budget := target - contextCost(c.ContextHeader)
		if breaksProtectedSpan(c.Start, c.Start, budget) || breaksProtectedSpan(c.End, c.Start, budget) {
			return reject("chunk %d at [%d,%d) cuts a protected span", i, c.Start, c.End)
		}
		n := c.End - c.Start
		if n > 2*target && !withinSpan(protected, runes, c) {
			return reject("chunk %d has %d runes, over twice the target size %d", i, n, target)
		}
		longest = max(longest, n)
		if i < len(chunks)-1 && n < tinyChunkRunes {
			tiny++
		}
	}
	if tiny > 2 && 4*tiny > len(chunks) {
		return reject("fragmented: %d of %d chunks are under %d runes", tiny, len(chunks), tinyChunkRunes)
	}
	if len(runes) > target && 4*longest < target {
		return reject("no chunk reaches a quarter of the target size %d", target)
	}
	return ValidationResult{OK: true}
}

// withinSpan reports whether c, less surrounding whitespace, lies inside a
// single protected span.
func withinSpan(spans []span, runes []rune, c Chunk) bool {
	start, end := c.Start, c.End
	if start < 0 || end > len(runes) {
		return false
	}
	for start < end && unicode.IsSpace(runes[start]) {
		start++
	}
	for end > start && unicode.IsSpace(runes[end-1]) {
		end--
	}
	i := firstEndingAfter(spans, start)
	return i < len(spans) && spans[i].start <= start && end <= spans[i].end
}
