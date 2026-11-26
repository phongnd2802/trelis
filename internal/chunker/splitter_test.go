package chunker

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func recursiveCfg(size, overlap int) SplitterConfig {
	return SplitterConfig{ChunkSize: size, ChunkOverlap: overlap, Strategy: StrategyRecursive}
}

// assertPositions checks the position invariant: every Chunk's Content is
// the source runes at [Start,End), and Seq counts up from 0.
func assertPositions(t *testing.T, text string, chunks []Chunk) {
	t.Helper()
	runes := []rune(text)
	for i, c := range chunks {
		if c.Seq != i {
			t.Fatalf("chunk %d has Seq %d", i, c.Seq)
		}
		if c.Start < 0 || c.End > len(runes) || c.Start >= c.End {
			t.Fatalf("chunk %d: bad offsets [%d,%d) for %d runes", i, c.Start, c.End, len(runes))
		}
		if got := string(runes[c.Start:c.End]); got != c.Content {
			t.Fatalf("chunk %d: runes[%d:%d] = %q, Content = %q", i, c.Start, c.End, got, c.Content)
		}
	}
}

// restore rebuilds the text from Chunks, skipping the overlap each Chunk
// repeats. Only whitespace may fall between Chunks.
func restore(t *testing.T, text string, chunks []Chunk) string {
	t.Helper()
	runes := []rune(text)
	var b strings.Builder
	prev := 0
	for i, c := range chunks {
		if c.Start > prev {
			if gap := string(runes[prev:c.Start]); strings.TrimSpace(gap) != "" {
				t.Fatalf("text %q before chunk %d is in no chunk", gap, i)
			}
			b.WriteString(string(runes[prev:c.Start]))
			prev = c.Start
		}
		if c.End > prev {
			b.WriteString(string(runes[prev:c.End]))
			prev = c.End
		}
	}
	b.WriteString(string(runes[prev:]))
	return b.String()
}

// assertWhole checks that span, which occurs once in text, lies inside one
// Chunk.
func assertWhole(t *testing.T, text, span string, chunks []Chunk) {
	t.Helper()
	if n := strings.Count(text, span); n != 1 {
		t.Fatalf("test setup: span occurs %d times", n)
	}
	for _, c := range chunks {
		if strings.Contains(c.Content, span) {
			return
		}
	}
	t.Fatalf("span %q is split across chunks %q", span, contents(chunks))
}

func contents(chunks []Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.Content
	}
	return out
}

func maxRunes(chunks []Chunk) int {
	best := 0
	for _, c := range chunks {
		best = max(best, utf8.RuneCountInString(c.Content))
	}
	return best
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

const table = `| Bậc | Thu nhập tính thuế/tháng | Thuế suất |
|-----|--------------------------|-----------|
| 1   | Đến 5 triệu đồng         | 5%        |
| 2   | Trên 5 đến 10 triệu đồng | 10%       |
| 3   | Trên 10 đến 18 triệu     | 15%       |`

func TestSplitEmptyText(t *testing.T) {
	if got := Split("", DefaultConfig()); got != nil {
		t.Fatalf("got %d chunks for empty text", len(got))
	}
	if got := Split(" \n\n\t ", DefaultConfig()); got != nil {
		t.Fatalf("got %d chunks for blank text", len(got))
	}
}

func TestSplitShortTextIsOneChunk(t *testing.T) {
	text := "Thuế thu nhập cá nhân là thuế trực thu."
	chunks := Split(text, recursiveCfg(100, 0))
	if len(chunks) != 1 || chunks[0].Content != text {
		t.Fatalf("chunks = %q, want the text as one chunk", contents(chunks))
	}
	assertPositions(t, text, chunks)
}

func TestSplitOffsetsAreRuneOffsets(t *testing.T) {
	text := "Điều 1. Phạm vi điều chỉnh.\n\nĐiều 2. Đối tượng nộp thuế."
	chunks := Split(text, recursiveCfg(40, 0))
	if len(chunks) != 2 {
		t.Fatalf("chunks = %q, want one per paragraph", contents(chunks))
	}
	assertPositions(t, text, chunks)
	if want := utf8.RuneCountInString(text); chunks[1].End != want {
		t.Fatalf("last chunk ends at %d, want rune length %d", chunks[1].End, want)
	}
}

func TestSplitRespectsTheChunkSize(t *testing.T) {
	tests := []struct {
		name string
		text string
		size int
	}{
		{"paragraphs", prose(8, 3), 200},
		{"one long paragraph", prose(1, 40), 150},
		{"one long line without sentence ends", strings.Repeat("thuế thu nhập ", 200), 100},
		{"one word longer than the size", strings.Repeat("đ", 1000), 64},
		{"Chinese", strings.Repeat("这是一段用于测试切分的中文内容。", 50), 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := Split(tt.text, recursiveCfg(tt.size, 0))
			if len(chunks) < 2 {
				t.Fatalf("got %d chunks, want the text split", len(chunks))
			}
			assertPositions(t, tt.text, chunks)
			if got := maxRunes(chunks); got > tt.size {
				t.Fatalf("largest chunk has %d runes, size is %d", got, tt.size)
			}
			if got := restore(t, tt.text, chunks); got != tt.text {
				t.Fatal("chunks do not restore the text")
			}
		})
	}
}

func TestSplitPrefersHigherPrioritySeparators(t *testing.T) {
	text := "one two three. four five six.\n\nseven eight nine. ten eleven twelve."
	chunks := Split(text, recursiveCfg(40, 0))
	want := []string{"one two three. four five six.\n\n", "seven eight nine. ten eleven twelve."}
	if got := contents(chunks); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("chunks = %q, want %q", got, want)
	}
}

func TestProtectedSpansAreNeverSplit(t *testing.T) {
	before, after := prose(3, 4), prose(2, 4)
	tests := []struct {
		name string
		span string
	}{
		{"pipe table", table},
		{"backtick fence with blank lines", "```go\nfunc main() {\n\tfmt.Println(\"a\")\n\n\tfmt.Println(\"b\")\n}\n```"},
		{"tilde fence holding a shorter fence", "~~~~\nline one\n\n~~~\nstill code\n~~~~"},
		{"fence holding table-like lines", "```\n| a | b |\n|---|---|\n\n| c | d |\n```"},
		{"math block", "$$\nT = \\sum_{i=1}^{n} r_i \\cdot x_i\n\n+ c\n$$"},
		{"inline code", "`go test ./internal/chunker/... -run TestSplit -count 1`"},
		{"link", "[Luật Thuế thu nhập cá nhân](https://thuvienphapluat.vn/van-ban/Thue-Phi-Le-Phi/Luat-thue-thu-nhap-ca-nhan-2007.aspx)"},
		{"image", "![Biểu thuế lũy tiến từng phần](images/bieu-thue-luy-tien-tung-phan.png)"},
	}
	for _, tt := range tests {
		for _, size := range []int{60, 150, 400} {
			t.Run(fmt.Sprintf("%s/size %d", tt.name, size), func(t *testing.T) {
				text := before + "\n\n" + tt.span + "\n" + after
				chunks := Split(text, recursiveCfg(size, size/5))
				assertPositions(t, text, chunks)
				assertWhole(t, text, tt.span, chunks)
			})
		}
	}
}

func TestProtectedSpansInDocumentOrder(t *testing.T) {
	parts := []string{
		"`ví dụ [link](url)`",
		"![hình](image.png)",
		"$$x + y$$",
		"| tên | giá trị |\n| --- | --- |\n| ô | `mã` |",
		"```go\n[inside](code)\n```",
		"[cuối](https://example.com)",
	}
	text := strings.Join(parts, "\n")
	var got []string
	last := 0
	for _, s := range protectedSpans(text) {
		if s.start < last {
			t.Fatalf("span %v overlaps or precedes the previous one ending at %d", s, last)
		}
		got = append(got, text[s.start:s.end])
		last = s.end
	}
	if strings.Join(got, "\x00") != strings.Join(parts, "\x00") {
		t.Fatalf("spans = %q, want %q", got, parts)
	}
}

func TestProtectedSpanDetection(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"inline code at line start is not a fence", "```x``` then text\nmore\n```\ncode\n```", []string{"```x```", "```\ncode\n```"}},
		{"an unclosed fence protects nothing", "```\nkhông đóng\n\nđoạn sau", nil},
		{"an unclosed math block protects nothing", "giá $$5 và hơn thế", nil},
		{"a link may not span lines", "[liên kết\nxuống dòng](http://example.com)", nil},
		{"a stray bracket does not swallow paragraphs", "[công văn】\n\nđoạn một.\n\nđoạn hai (Hà Nội)", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, s := range protectedSpans(tt.text) {
				got = append(got, tt.text[s.start:s.end])
			}
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("spans = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStrayBracketDoesNotSwallowTheDocument(t *testing.T) {
	var b strings.Builder
	b.WriteString("[công văn】\n\n")
	for i := range 60 {
		fmt.Fprintf(&b, "Đây là đoạn thứ %d dùng để kiểm tra việc cắt văn bản.\n\n", i)
	}
	b.WriteString("[trích dẫn](nguồn)")
	text := b.String()
	chunks := Split(text, recursiveCfg(128, 0))
	if len(chunks) < 2 || maxRunes(chunks) > 128 {
		t.Fatalf("got %d chunks, largest %d runes: the stray bracket swallowed the text", len(chunks), maxRunes(chunks))
	}
}

func TestAProtectedSpanOverTheChunkSizeStaysWhole(t *testing.T) {
	text := "Mở đầu.\n\n" + table + "\n\nKết thúc."
	chunks := Split(text, recursiveCfg(60, 0))
	assertPositions(t, text, chunks)
	assertWhole(t, text, table, chunks)
}

func TestTokenLimitCapsEveryChunkIncludingProtectedSpans(t *testing.T) {
	var rows strings.Builder
	rows.WriteString("| Bậc | Thuế suất |\n|---|---|\n")
	for i := range 80 {
		fmt.Fprintf(&rows, "| %d | %d%% |\n", i, i)
	}
	text := prose(4, 4) + "\n\n" + rows.String() + "\n" + prose(2, 3)
	cfg := SplitterConfig{TokenLimit: 100, Languages: []string{LangVietnamese}, Strategy: StrategyRecursive}
	budget := CharsForTokenLimit(100, LangVietnamese)

	chunks := Split(text, cfg)
	assertPositions(t, text, chunks)
	if got := maxRunes(chunks); got > budget {
		t.Fatalf("largest chunk has %d runes, the token limit allows %d", got, budget)
	}
}

func TestOverlapStartsAtASemanticBoundary(t *testing.T) {
	text := prose(6, 6)
	chunks := Split(text, recursiveCfg(200, 80))
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want several", len(chunks))
	}
	assertPositions(t, text, chunks)
	runes := []rune(text)
	overlapped := 0
	for i := 1; i < len(chunks); i++ {
		prev, c := chunks[i-1], chunks[i]
		if c.Start >= prev.End {
			continue
		}
		overlapped++
		if n := prev.End - c.Start; n > 80 {
			t.Errorf("chunk %d repeats %d runes, overlap is 80", i, n)
		}
		// The overlap starts right after a sentence, line or paragraph end.
		before := string(runes[max(0, c.Start-2):c.Start])
		if !strings.HasSuffix(before, ". ") && !strings.HasSuffix(before, "\n") {
			t.Errorf("chunk %d overlap starts after %q, not at a boundary", i, before)
		}
	}
	if overlapped == 0 {
		t.Fatal("no chunk overlaps the one before it")
	}
	if maxRunes(chunks) > 200 {
		t.Fatalf("largest chunk has %d runes, size is 200", maxRunes(chunks))
	}
	if got := restore(t, text, chunks); got != text {
		t.Fatal("chunks do not restore the text")
	}
}

func TestNoBoundaryMeansNoOverlap(t *testing.T) {
	text := strings.Repeat("abcdefghij", 50)
	chunks := Split(text, recursiveCfg(100, 40))
	for i := 1; i < len(chunks); i++ {
		if chunks[i].Start < chunks[i-1].End {
			t.Fatalf("chunk %d overlaps without a boundary to start at", i)
		}
	}
}

func TestOverlapNeverCutsAProtectedSpan(t *testing.T) {
	url := "Xem https://example.com/search?q=thuế. Tiếp theo [liên kết](https://example.com/x?y=1&z=2) và câu cuối."
	text := strings.Repeat(url+" ", 8)
	chunks := Split(text, recursiveCfg(120, 50))
	for i, c := range chunks {
		if strings.HasPrefix(c.Content, "(https") || strings.HasPrefix(c.Content, "https://example.com/x") {
			t.Fatalf("chunk %d starts inside a link: %q", i, c.Content)
		}
	}
}

func TestEmbeddingContent(t *testing.T) {
	c := Chunk{Content: "\n  nội dung  \n"}
	if got := c.EmbeddingContent(); got != "nội dung" {
		t.Fatalf("got %q", got)
	}
	c.ContextHeader = "Chương I > Điều 1"
	if got := c.EmbeddingContent(); got != "Chương I > Điều 1\n\nnội dung" {
		t.Fatalf("got %q", got)
	}
}
