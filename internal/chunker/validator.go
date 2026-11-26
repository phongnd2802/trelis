package chunker

import (
	"fmt"
	"strings"
)

// Validator decides whether a tier's Chunks are good enough to keep. A
// non-nil error rejects them, and the Chunker falls through to the next
// tier. The error says why, for Result.Rejected.
type Validator func(in Input, chunks []Chunk, size Sizer) error

// Validate is the default Validator. It rejects Chunks that
//
//   - are empty, or whose Content is not text[Start:End];
//   - are out of order or overlap;
//   - leave out anything but whitespace;
//   - start or end inside a protected span;
//   - are over the size cap, unless the Chunk is one protected span that
//     is over the cap on its own.
func Validate(in Input, chunks []Chunk, size Sizer) error {
	if len(chunks) == 0 {
		return fmt.Errorf("no chunks")
	}
	protected := spans(in.Protected)
	prev := 0
	for i, ch := range chunks {
		if ch.Start < prev || ch.End > len(in.Text) || ch.Start >= ch.End {
			return fmt.Errorf("chunk %d: offsets [%d,%d) are empty, out of order or out of range", i, ch.Start, ch.End)
		}
		if in.Text[ch.Start:ch.End] != ch.Content {
			return fmt.Errorf("chunk %d: Content is not the text at [%d,%d)", i, ch.Start, ch.End)
		}
		if strings.TrimSpace(in.Text[prev:ch.Start]) != "" {
			return fmt.Errorf("chunk %d: text before it at [%d,%d) is in no chunk", i, prev, ch.Start)
		}
		if protected.inside(ch.Start) || protected.inside(ch.End) {
			return fmt.Errorf("chunk %d: [%d,%d) cuts a protected span", i, ch.Start, ch.End)
		}
		if !size.Fits(ch.Content) && !protected.only(in.Text, ch.Start, ch.End) {
			return fmt.Errorf("chunk %d: %d tokens is over the cap of %d", i, size.Tokens(ch.Content), size.Limit())
		}
		prev = ch.End
	}
	if strings.TrimSpace(in.Text[prev:]) != "" {
		return fmt.Errorf("text after the last chunk, from %d, is in no chunk", prev)
	}
	return nil
}
