package chunker_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/phongnd2802/trelis/internal/chunker"
)

// assertWhole checks that span, which occurs once in text, lies inside a
// single Chunk (up to surrounding whitespace, which Chunks leave out).
func assertWhole(t *testing.T, text, span string, chunks []chunker.Chunk) {
	t.Helper()
	span = strings.TrimSpace(span)
	if strings.Count(text, span) != 1 {
		t.Fatalf("test setup: span occurs %d times in text", strings.Count(text, span))
	}
	at := strings.Index(text, span)
	for _, ch := range chunks {
		if ch.Start <= at && at+len(span) <= ch.End {
			return
		}
	}
	t.Fatalf("span %q is split across chunks %q", span, contents(chunks))
}

const table = `| Bậc | Thu nhập tính thuế/tháng | Thuế suất |
|-----|--------------------------|-----------|
| 1   | Đến 5 triệu đồng         | 5%        |
| 2   | Trên 5 đến 10 triệu đồng | 10%       |
| 3   | Trên 10 đến 18 triệu     | 15%       |`

func TestProtectedSpansAreNeverSplit(t *testing.T) {
	before := prose(3, 4)
	after := prose(2, 4)
	tests := []struct {
		name string
		span string
	}{
		{"pipe table", table},
		{"table without outer pipes", "Bậc | Thuế suất\n--- | ---\n1 | 5%\n2 | 10%"},
		{"backtick fence with blank lines", "```go\nfunc main() {\n\tfmt.Println(\"a\")\n\n\tfmt.Println(\"b\")\n}\n```"},
		{"tilde fence holding a shorter fence", "~~~~\nline one\n\n~~~\nstill code\n~~~~"},
		{"fence holding table-like lines", "```\n| a | b |\n|---|---|\n\n| c | d |\n```"},
		{"indented fence", "  ```\n  x := 1\n\n  y := 2\n  ```"},
		{"dollar math block", "$$\nT = \\sum_{i=1}^{n} r_i \\cdot x_i\n\n+ c\n$$"},
		{"bracket math block", "\\[\na^2 + b^2 = c^2\n\n\\]"},
	}
	for _, tt := range tests {
		for _, limit := range []int{8, 20, 60} {
			t.Run(fmt.Sprintf("%s/limit %d", tt.name, limit), func(t *testing.T) {
				text := before + "\n\n" + tt.span + "\n\n" + after
				res := mustNew(t, chunker.Config{TokenLimit: limit, CountTokens: words}).Chunk(text)
				assertOffsets(t, text, res.Chunks)
				assertCovers(t, text, res.Chunks)
				assertWhole(t, text, tt.span, res.Chunks)
			})
		}
	}
}

func TestProtectedSpanMidParagraphIsNotSplit(t *testing.T) {
	text := "Bảng dưới đây liệt kê biểu thuế lũy tiến từng phần:\n" + table + "\nNguồn: Luật Thuế TNCN."
	res := mustNew(t, chunker.Config{TokenLimit: 12, CountTokens: words}).Chunk(text)
	assertOffsets(t, text, res.Chunks)
	assertCovers(t, text, res.Chunks)
	assertWhole(t, text, table, res.Chunks)
}

func TestAProtectedSpanOverTheCapIsItsOwnChunk(t *testing.T) {
	text := "Mở đầu.\n\n" + table + "\n\nKết thúc."
	res := mustNew(t, chunker.Config{TokenLimit: 5, CountTokens: words}).Chunk(text)
	want := []string{"Mở đầu.", table, "Kết thúc."}
	if got := contents(res.Chunks); !slices.Equal(got, want) {
		t.Fatalf("chunks = %q, want %q", got, want)
	}
}

func TestProtectedSpans(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "inline code at line start is not a fence",
			text: "```x``` then text\nmore\n```\ncode\n```",
			want: []string{"```\ncode\n```"},
		},
		{
			name: "prose between dollar amounts is not math",
			text: "$$5 is cheap\nand so on, end$$",
			want: nil,
		},
		{
			name: "one-line math block",
			text: "Tổng:\n$$ a + b $$\nhết.",
			want: []string{"$$ a + b $$"},
		},
		{
			name: "table inside a fence is code",
			text: "```\n| a | b |\n|---|---|\n```",
			want: []string{"```\n| a | b |\n|---|---|\n```"},
		},
		{
			name: "a lone pipe row is not a table",
			text: "| chỉ một dòng |\nvăn bản",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, s := range chunker.ProtectedSpans(tt.text) {
				got = append(got, tt.text[s.Start:s.End])
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("spans = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAnUnclosedFenceDoesNotProtectTheRestOfTheText(t *testing.T) {
	text := "```\n" + prose(4, 4)
	res := mustNew(t, chunker.Config{TokenLimit: 20, CountTokens: words}).Chunk(text)
	if len(res.Chunks) < 2 {
		t.Fatalf("got %d chunks, want the text after a stray fence split", len(res.Chunks))
	}
	for i, ch := range res.Chunks {
		if n := words(ch.Content); n > 20 {
			t.Errorf("chunk %d has %d tokens, cap is 20", i, n)
		}
	}
}
