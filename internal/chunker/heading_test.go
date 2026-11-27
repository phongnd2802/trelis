package chunker

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// wantChunk is one expected Chunk of a heading-tier test: its exact Content
// and the breadcrumb it must carry.
type wantChunk struct {
	content    string
	breadcrumb string
}

// splitHeadingText runs the heading tier with size and no overlap.
func splitHeadingText(text string, size int) []Chunk {
	cfg := ensureDefaults(SplitterConfig{ChunkSize: size, ChunkOverlap: 0}, text)
	return splitHeading(text, cfg)
}

// assertHeadingChunks checks exact Contents, breadcrumbs and the position
// invariant.
func assertHeadingChunks(t *testing.T, text string, got []Chunk, want []wantChunk) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d chunks %q, want %d", len(got), contents(got), len(want))
	}
	for i, c := range got {
		if c.Content != want[i].content {
			t.Errorf("chunk %d Content = %q, want %q", i, c.Content, want[i].content)
		}
		if c.ContextHeader != want[i].breadcrumb {
			t.Errorf("chunk %d ContextHeader = %q, want %q", i, c.ContextHeader, want[i].breadcrumb)
		}
	}
	assertPositions(t, text, got)
}

func TestHeadingTierSplitsAtHeadings(t *testing.T) {
	tests := []struct {
		name string
		text string
		size int
		want []wantChunk
	}{
		{
			name: "nested headings and breadcrumbs",
			text: "# Chương I\n\nĐoạn một.\n\n## Điều 1. Phạm vi\n\nNội dung điều một.\n\n### Khoản 1\n\nNội dung khoản một.",
			size: 200,
			want: []wantChunk{
				{"# Chương I\n\nĐoạn một.\n\n", ""},
				{"## Điều 1. Phạm vi\n\nNội dung điều một.\n\n", "Chương I"},
				{"### Khoản 1\n\nNội dung khoản một.", "Chương I > Điều 1. Phạm vi"},
			},
		},
		{
			name: "a skipped level just nests",
			text: "# A\n\nmột.\n\n### C\n\nhai.",
			size: 200,
			want: []wantChunk{
				{"# A\n\nmột.\n\n", ""},
				{"### C\n\nhai.", "A"},
			},
		},
		{
			name: "preamble before the first heading",
			text: "Mở đầu.\n\n# A\n\nthân bài.",
			size: 200,
			want: []wantChunk{
				{"Mở đầu.\n\n", ""},
				{"# A\n\nthân bài.", ""},
			},
		},
		{
			name: "closing hashes are stripped",
			text: "# Cha\n\n## Điều 1 ##\n\nNội dung.",
			size: 200,
			want: []wantChunk{
				{"## Điều 1 ##\n\nNội dung.", "Cha"},
			},
		},
		{
			name: "a heading line inside a fence is not a heading",
			text: "# Tiêu đề\n\n```\n# bình luận\nx := 1\n```\n\nNội dung.",
			size: 200,
			want: []wantChunk{
				{"# Tiêu đề\n\n```\n# bình luận\nx := 1\n```\n\nNội dung.", ""},
			},
		},
		{
			name: "setext headings are out of scope",
			text: "Tiêu đề\n========\n\nNội dung.",
			size: 200,
			want: []wantChunk{
				{"Tiêu đề\n========\n\nNội dung.", ""},
			},
		},
		{
			name: "a section at the size is one chunk",
			text: "# A\n\nbody",
			size: 9,
			want: []wantChunk{
				{"# A\n\nbody", ""},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertHeadingChunks(t, tt.text, splitHeadingText(tt.text, tt.size), tt.want)
		})
	}
}

func TestHeadingTierBodylessHeadingFeedsBreadcrumbsOnly(t *testing.T) {
	text := "# Phần\n\n## Điều 1\n\nNội dung."
	chunks := splitHeadingText(text, 200)
	assertHeadingChunks(t, text, chunks, []wantChunk{
		{"## Điều 1\n\nNội dung.", "Phần"},
	})
	for _, c := range chunks {
		if strings.Contains(c.Content, "# Phần") {
			t.Fatalf("a body-less heading got a Chunk: %q", c.Content)
		}
	}
}

func TestHeadingTierSplitsALongSectionWithinItsBreadcrumb(t *testing.T) {
	body := prose(4, 5)
	text := "# Chương I\n\n" + body + "\n\n## Điều 1\n\nNgắn gọn."
	cfg := ensureDefaults(SplitterConfig{ChunkSize: 180, ChunkOverlap: 40}, text)
	chunks := splitHeading(text, cfg)

	assertPositions(t, text, chunks)
	boundary := utf8.RuneCountInString("# Chương I\n\n" + body + "\n\n")
	if boundary >= utf8.RuneCountInString(text) {
		t.Fatal("test setup: no section after the boundary")
	}
	if got := maxRunes(chunks); got > cfg.ChunkSize {
		t.Fatalf("largest chunk has %d runes, size is %d", got, cfg.ChunkSize)
	}
	if chunks[0].Start != 0 {
		t.Fatalf("first chunk starts at %d, want 0", chunks[0].Start)
	}
	if last := chunks[len(chunks)-1]; last.End != utf8.RuneCountInString(text) {
		t.Fatalf("last chunk ends at %d, want %d", last.End, utf8.RuneCountInString(text))
	}

	inFirst := 0
	for i, c := range chunks {
		switch {
		case c.End <= boundary:
			inFirst++
			wantHeader := "Chương I"
			if c.Start == 0 {
				wantHeader = ""
			}
			if c.ContextHeader != wantHeader {
				t.Errorf("chunk %d in the long section has ContextHeader %q, want %q", i, c.ContextHeader, wantHeader)
			}
			if c.Start == 0 && !strings.HasPrefix(c.Content, "# Chương I") {
				t.Errorf("first chunk does not start with its heading line: %q", c.Content)
			}
		case c.Start >= boundary:
			if c.ContextHeader != "Chương I" {
				t.Errorf("chunk %d after the boundary has ContextHeader %q, want %q", i, c.ContextHeader, "Chương I")
			}
		default:
			t.Fatalf("chunk %d [%d,%d) crosses the section boundary at %d", i, c.Start, c.End, boundary)
		}
	}
	if inFirst < 2 {
		t.Fatalf("the long section produced %d chunks, want it split", inFirst)
	}
	atBoundary := false
	for _, c := range chunks {
		if c.Start == boundary {
			atBoundary = true
		}
	}
	if !atBoundary {
		t.Fatal("no chunk starts at the next section's heading line")
	}
}

func TestHeadingTierCountsRunesNotBytes(t *testing.T) {
	text := "# Chương I\n\nNội dung chương.\n\n## Điều 1. Phạm vi\n\nNội dung điều."
	chunks := splitHeadingText(text, 100)
	assertHeadingChunks(t, text, chunks, []wantChunk{
		{"# Chương I\n\nNội dung chương.\n\n", ""},
		{"## Điều 1. Phạm vi\n\nNội dung điều.", "Chương I"},
	})
	last := chunks[len(chunks)-1]
	if want := utf8.RuneCountInString(text); last.End != want {
		t.Fatalf("last chunk ends at %d, want the rune length %d", last.End, want)
	}
	if last.End >= len(text) {
		t.Fatalf("last chunk ends at %d, not before the byte length %d: offsets are byte offsets", last.End, len(text))
	}
}

func TestHeadingTierContextHeaderOnlyAddsWhatContentLacks(t *testing.T) {
	text := "# Chương I\n\nMở đầu.\n\n## Điều 1. Phạm vi\n\nNội dung điều."
	chunks := splitHeadingText(text, 200)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks %q, want 2", len(chunks), contents(chunks))
	}
	if h := chunks[0].ContextHeader; h != "" {
		t.Errorf("top-level section ContextHeader = %q, want empty", h)
	}
	if h := chunks[1].ContextHeader; h != "Chương I" {
		t.Errorf("nested section ContextHeader = %q, want its ancestor %q", h, "Chương I")
	}
	if !strings.HasPrefix(chunks[0].Content, "# Chương I") {
		t.Errorf("first chunk does not start with its heading line: %q", chunks[0].Content)
	}
	if !strings.HasPrefix(chunks[1].Content, "## Điều 1. Phạm vi") {
		t.Errorf("second chunk does not start with its heading line: %q", chunks[1].Content)
	}
	if n := strings.Count(chunks[0].EmbeddingContent(), "Chương I"); n != 1 {
		t.Errorf("top-level heading appears %d times in the embedded text", n)
	}
	if n := strings.Count(chunks[1].EmbeddingContent(), "Điều 1. Phạm vi"); n != 1 {
		t.Errorf("nested heading appears %d times in the embedded text", n)
	}
}

func TestHeadingTierContextHeaderUsesTheRealAncestors(t *testing.T) {
	text := "# A\n\nthân A.\n\n## B\n\nthân B.\n\n## C\n\nthân C."
	chunks := splitHeadingText(text, 200)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks %q, want 3", len(chunks), contents(chunks))
	}
	want := []string{"", "A", "A"}
	for i, c := range chunks {
		if c.ContextHeader != want[i] {
			t.Errorf("chunk %d ContextHeader = %q, want %q (a sibling is not an ancestor)", i, c.ContextHeader, want[i])
		}
	}
}

func TestHeadingTierCountsTheBreadcrumbInTheBudget(t *testing.T) {
	text := "# Chương I. Thuế thu nhập\n\n## Điều 1. Phạm vi điều chỉnh\n\n" +
		strings.Repeat("Người nộp thuế là cá nhân cư trú và cá nhân không cư trú. ", 20)
	cfg := ensureDefaults(SplitterConfig{ChunkSize: 150, ChunkOverlap: 0}, text)
	chunks := splitHeading(text, cfg)
	assertPositions(t, text, chunks)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want the long section split", len(chunks))
	}

	boundary := utf8.RuneCountInString(text[:strings.Index(text, "## Điều")])
	ancestors := "Chương I. Thuế thu nhập"
	full := ancestors + " > Điều 1. Phạm vi điều chỉnh"
	if chunks[0].Start != boundary {
		t.Fatalf("first chunk starts at %d, want the section at %d", chunks[0].Start, boundary)
	}
	if !strings.HasPrefix(chunks[0].Content, "## Điều 1. Phạm vi điều chỉnh") {
		t.Fatalf("first chunk does not start with its heading line: %q", chunks[0].Content)
	}
	for i, c := range chunks {
		if got := utf8.RuneCountInString(c.EmbeddingContent()); got > cfg.ChunkSize {
			t.Errorf("chunk %d embeds %d runes, budget is %d", i, got, cfg.ChunkSize)
		}
		if c.Start == boundary {
			if c.ContextHeader != ancestors {
				t.Errorf("first chunk ContextHeader = %q, want ancestors %q", c.ContextHeader, ancestors)
			}
		} else {
			if c.ContextHeader != full {
				t.Errorf("later chunk ContextHeader = %q, want the full breadcrumb %q", c.ContextHeader, full)
			}
			if strings.HasPrefix(c.Content, "## Điều 1. Phạm vi điều chỉnh") {
				t.Errorf("later chunk still starts with the section heading line: %q", c.Content)
			}
		}
	}
}

func TestSectionBudget(t *testing.T) {
	tests := []struct {
		name   string
		size   int
		header string
		want   int
	}{
		{"no header", 120, "", 120},
		{"a header is deducted with its blank line", 120, strings.Repeat("x", 30), 88},
		{"a huge header keeps half the size", 120, strings.Repeat("x", 200), 60},
		{"a tiny size never reaches zero", 1, strings.Repeat("x", 5), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sectionBudget(SplitterConfig{ChunkSize: tt.size}, tt.header); got != tt.want {
				t.Fatalf("sectionBudget(size %d, header %d runes) = %d, want %d", tt.size, utf8.RuneCountInString(tt.header), got, tt.want)
			}
		})
	}
}

func TestHeadingTierKeepsTheDocumentTokenBudget(t *testing.T) {
	var rows strings.Builder
	rows.WriteString("| Row | Value |\n|---|---|\n")
	for i := range 60 {
		fmt.Fprintf(&rows, "| row %d | value %d |\n", i, i)
	}
	text := prose(2, 3) + "\n\n# Data\n\n" + rows.String()
	cfg := ensureDefaults(SplitterConfig{TokenLimit: 100, Strategy: StrategyAuto}, text)
	if lang := tokenLanguage(cfg, text); lang != LangVietnamese {
		t.Fatalf("test setup: the document detects as %q, want Vietnamese", lang)
	}

	chunks := splitHeading(text, cfg)
	assertPositions(t, text, chunks)
	if budget := CharsForTokenLimit(100, LangVietnamese); maxRunes(chunks) > budget {
		t.Fatalf("largest chunk has %d runes, the token limit allows %d", maxRunes(chunks), budget)
	}
}

func TestParseATXHeading(t *testing.T) {
	tests := []struct {
		line  string
		level int
		text  string
		ok    bool
	}{
		{"# A", 1, "A", true},
		{"###### Sáu", 6, "Sáu", true},
		{"####### Bảy", 0, "", false},
		{"#Không có dấu cách", 0, "", false},
		{"   ### Ba dấu cách", 3, "Ba dấu cách", true},
		{"    # Bốn dấu cách", 0, "", false},
		{"## Đóng ##", 2, "Đóng", true},
		{"## Không đóng#", 2, "Không đóng#", true},
		{"###", 3, "", true},
		{"## ###", 2, "", true},
		{"#\tTab", 1, "Tab", true},
		{"# A\r", 1, "A", true},
		{"", 0, "", false},
	}
	for _, tt := range tests {
		level, text, ok := parseATXHeading(tt.line)
		if level != tt.level || text != tt.text || ok != tt.ok {
			t.Errorf("parseATXHeading(%q) = (%d, %q, %v), want (%d, %q, %v)",
				tt.line, level, text, ok, tt.level, tt.text, tt.ok)
		}
	}
}

func TestHeadingsSkipProtectedSpans(t *testing.T) {
	text := "# Một\n\n```\n# hai\n```\n\n`# ba`\n\n## Bốn ##"
	got := headings(text)
	if len(got) != 2 {
		t.Fatalf("headings = %+v, want only the two outside code", got)
	}
	if got[0].level != 1 || got[0].text != "Một" || got[0].start != 0 {
		t.Errorf("first heading = %+v, want level 1 %q at 0", got[0], "Một")
	}
	wantStart := strings.Index(text, "## Bốn")
	if got[1].level != 2 || got[1].text != "Bốn" || got[1].start != wantStart {
		t.Errorf("second heading = %+v, want level 2 %q at %d", got[1], "Bốn", wantStart)
	}
}
