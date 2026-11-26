package chunker

import (
	"fmt"
	"unicode/utf8"
)

// tinyChunkRunes is the size under which a Chunk, other than the last,
// counts as tiny.
const tinyChunkRunes = 50

// ValidationResult is ValidateChunks' verdict on one tier's output.
type ValidationResult struct {
	OK     bool
	Reason string
}

// ValidateChunks decides whether a tier's Chunks for text are usable or the
// chain should fall through to the next tier. It is deliberately lenient
// about size, rejecting only output that is plainly broken:
//
//   - no Chunks;
//   - a Chunk whose Content is not the source at its offsets, or that
//     starts or ends inside a protected span;
//   - one Chunk for text over twice chunkSize (the tier did not split);
//   - more than a quarter of the Chunks (and more than two) tiny, the last
//     one aside (the tier fragmented the text);
//   - no Chunk reaching a quarter of chunkSize, for text over chunkSize;
//   - a Chunk over twice chunkSize (the tier ignored its budget).
func ValidateChunks(text string, chunks []Chunk, chunkSize int) ValidationResult {
	if len(chunks) == 0 {
		return ValidationResult{Reason: "no chunks produced"}
	}
	runes := []rune(text)
	protected := toRuneSpans(text, protectedSpans(text))
	longest, tiny := 0, 0
	for i, c := range chunks {
		if c.Start < 0 || c.End > len(runes) || c.Start >= c.End || string(runes[c.Start:c.End]) != c.Content {
			return ValidationResult{Reason: fmt.Sprintf("chunk %d is not the source text at [%d,%d)", i, c.Start, c.End)}
		}
		if insideSpan(protected, c.Start) || insideSpan(protected, c.End) {
			return ValidationResult{Reason: fmt.Sprintf("chunk %d at [%d,%d) cuts a protected span", i, c.Start, c.End)}
		}
		n := utf8.RuneCountInString(c.Content)
		longest = max(longest, n)
		if n < tinyChunkRunes && i < len(chunks)-1 {
			tiny++
		}
	}
	switch {
	case len(chunks) == 1 && len(runes) > 2*chunkSize:
		return ValidationResult{Reason: "single chunk for large document"}
	case tiny > len(chunks)/4 && tiny > 2:
		return ValidationResult{Reason: "too many tiny chunks"}
	case longest < chunkSize/4 && len(runes) > chunkSize:
		return ValidationResult{Reason: "all chunks far below target size"}
	case longest > 2*chunkSize:
		return ValidationResult{Reason: "chunk exceeds 2x target size"}
	}
	return ValidationResult{OK: true}
}
