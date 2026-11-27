package modeltest

import (
	"testing"

	"github.com/phongnd2802/trelis/internal/types"
)

// Collect drains a stream into the chunk sequence its consumer sees.
func Collect(t *testing.T, ch <-chan types.StreamResponse) []types.StreamResponse {
	t.Helper()
	var chunks []types.StreamResponse
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	return chunks
}

// CloseIndex returns the position of the stream's one closing chunk: the
// answer chunk with Done set, or the error chunk of a broken stream. The
// thinking-done marker also sets Done but is not a close, so it is ignored.
// The test fails when the stream has no closing chunk or more than one.
func CloseIndex(t *testing.T, chunks []types.StreamResponse) int {
	t.Helper()
	idx := -1
	for i, chunk := range chunks {
		if !chunk.Done || chunk.ResponseType == types.ResponseTypeThinking {
			continue
		}
		if idx >= 0 {
			t.Fatalf("chunks %d and %d both close the stream, want exactly one: %+v", idx, i, chunks)
		}
		idx = i
	}
	if idx < 0 {
		t.Fatalf("no chunk closes the stream: %+v", chunks)
	}
	return idx
}
