package chunker_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/phongnd2802/trelis/internal/chunker"
)

func mustNew(t *testing.T, cfg chunker.Config) *chunker.Chunker {
	t.Helper()
	c, err := chunker.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// assertOffsets checks that every Chunk points back at its own text.
func assertOffsets(t *testing.T, text string, chunks []chunker.Chunk) {
	t.Helper()
	for i, ch := range chunks {
		if ch.Start < 0 || ch.End > len(text) || ch.Start >= ch.End {
			t.Fatalf("chunk %d: bad offsets [%d,%d) for text of %d bytes", i, ch.Start, ch.End, len(text))
		}
		if got := text[ch.Start:ch.End]; got != ch.Content {
			t.Fatalf("chunk %d: text[%d:%d] = %q, Content = %q", i, ch.Start, ch.End, got, ch.Content)
		}
	}
}

// assertCovers checks that the Chunks are in order, do not overlap, and
// leave out nothing but whitespace.
func assertCovers(t *testing.T, text string, chunks []chunker.Chunk) {
	t.Helper()
	prev := 0
	for i, ch := range chunks {
		if ch.Start < prev {
			t.Fatalf("chunk %d starts at %d, before the previous chunk ended at %d", i, ch.Start, prev)
		}
		if gap := text[prev:ch.Start]; strings.TrimSpace(gap) != "" {
			t.Fatalf("text %q before chunk %d is in no chunk", gap, i)
		}
		prev = ch.End
	}
	if tail := text[prev:]; strings.TrimSpace(tail) != "" {
		t.Fatalf("trailing text %q is in no chunk", tail)
	}
}

// words counts whitespace-separated words, a token counter tests can
// reason about by eye.
func words(s string) int { return len(strings.Fields(s)) }

func contents(chunks []chunker.Chunk) []string {
	out := make([]string, len(chunks))
	for i, ch := range chunks {
		out[i] = ch.Content
	}
	return out
}

func prose(paragraphs, sentences int) string {
	var b strings.Builder
	for p := range paragraphs {
		if p > 0 {
			b.WriteString("\n\n")
		}
		for s := range sentences {
			if s > 0 {
				b.WriteString(" ")
			}
			fmt.Fprintf(&b, "Câu số %d của đoạn %d nói về thuế thu nhập cá nhân.", s, p)
		}
	}
	return b.String()
}

func TestShortTextIsOneChunk(t *testing.T) {
	text := "\n  Thuế thu nhập cá nhân là thuế trực thu.  \n"
	res := mustNew(t, chunker.Config{TokenLimit: 512}).Chunk(text)

	if len(res.Chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(res.Chunks))
	}
	if want := "Thuế thu nhập cá nhân là thuế trực thu."; res.Chunks[0].Content != want {
		t.Fatalf("Content = %q, want %q", res.Chunks[0].Content, want)
	}
	assertOffsets(t, text, res.Chunks)
}

func TestUnicodeSpacesAreWhitespace(t *testing.T) {
	text := "Đoạn một.\n\n 　 \n\nĐoạn hai."
	res := mustNew(t, chunker.Config{TokenLimit: 3, CountTokens: words}).Chunk(text)
	want := []string{"Đoạn một.", "Đoạn hai."}
	if got := contents(res.Chunks); !slices.Equal(got, want) {
		t.Fatalf("chunks = %q, want %q", got, want)
	}
}

func TestEmptyTextHasNoChunks(t *testing.T) {
	for _, text := range []string{"", " \n\t\n", " 　"} {
		if res := mustNew(t, chunker.Config{TokenLimit: 512}).Chunk(text); len(res.Chunks) != 0 {
			t.Fatalf("Chunk(%q) gave %d chunks, want 0", text, len(res.Chunks))
		}
	}
}

func TestChunksRespectTheSizeCap(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
	}{
		{"paragraphs fit", prose(6, 3), 40},
		{"paragraph larger than the cap", prose(2, 20), 25},
		{"sentence larger than the cap", strings.Repeat("một hai ba bốn năm ", 30), 7},
		{"single line, no sentence ends", strings.Repeat("abc ", 200), 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := mustNew(t, chunker.Config{TokenLimit: tt.limit, CountTokens: words}).Chunk(tt.text)
			if len(res.Chunks) < 2 {
				t.Fatalf("got %d chunks, want the text split", len(res.Chunks))
			}
			assertOffsets(t, tt.text, res.Chunks)
			assertCovers(t, tt.text, res.Chunks)
			for i, ch := range res.Chunks {
				if n := words(ch.Content); n > tt.limit {
					t.Errorf("chunk %d has %d tokens, cap is %d: %q", i, n, tt.limit, ch.Content)
				}
			}
		})
	}
}

func TestAWordLargerThanTheCapIsCutAtRuneBoundaries(t *testing.T) {
	text := strings.Repeat("đ", 2000)
	const limit = 50
	res := mustNew(t, chunker.Config{TokenLimit: limit}).Chunk(text)
	if len(res.Chunks) < 2 {
		t.Fatalf("got %d chunks, want the word cut", len(res.Chunks))
	}
	assertOffsets(t, text, res.Chunks)
	assertCovers(t, text, res.Chunks)
	for i, ch := range res.Chunks {
		if !utf8.ValidString(ch.Content) {
			t.Fatalf("chunk %d is cut inside a rune", i)
		}
		if n := chunker.EstimateTokens(ch.Content); n > limit {
			t.Errorf("chunk %d has an estimated %d tokens, limit is %d", i, n, limit)
		}
	}
}

func TestDefaultCounterKeepsChunksUnderTheLimit(t *testing.T) {
	text := prose(30, 8)
	const limit = 128
	res := mustNew(t, chunker.Config{TokenLimit: limit}).Chunk(text)
	if len(res.Chunks) < 2 {
		t.Fatalf("got %d chunks, want the text split", len(res.Chunks))
	}
	assertOffsets(t, text, res.Chunks)
	assertCovers(t, text, res.Chunks)
	for i, ch := range res.Chunks {
		if n := chunker.EstimateTokens(ch.Content); n > limit {
			t.Errorf("chunk %d has an estimated %d tokens, limit is %d", i, n, limit)
		}
	}
}

func TestParagraphBoundariesArePreferred(t *testing.T) {
	// Two paragraphs of 6 words each; a 9-word cap fits one paragraph but
	// not both, so the split falls between them rather than mid-sentence.
	text := "one two three. four five six.\n\nseven eight nine. ten eleven twelve."
	res := mustNew(t, chunker.Config{TokenLimit: 9, CountTokens: words}).Chunk(text)
	want := []string{"one two three. four five six.", "seven eight nine. ten eleven twelve."}
	if got := contents(res.Chunks); !slices.Equal(got, want) {
		t.Fatalf("chunks = %q, want %q", got, want)
	}
}
