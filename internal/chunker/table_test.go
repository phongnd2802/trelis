package chunker

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// rowsTable makes a table with a header and n short body rows.
func rowsTable(n int) string {
	var b strings.Builder
	b.WriteString("| Bậc | Thuế suất |\n|---|---|\n")
	for i := range n {
		fmt.Fprintf(&b, "| %d | %d%% |\n", i, i)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// tableHeaderOf returns the header of the only table in text: its first row
// and delimiter row, or empty for a table without one.
func tableHeaderOf(t *testing.T, text string) string {
	t.Helper()
	spans := tableSpans(text, 0, len(text))
	if len(spans) != 1 {
		t.Fatalf("test setup: found %d tables, want one", len(spans))
	}
	return tableHeader(text, tableRows(text, spans[0]))
}

func TestTableHeaderCarriesOver(t *testing.T) {
	tests := []struct {
		name string
		text string
		cfg  SplitterConfig
		// whole checks that the table const stays inside one Chunk.
		whole bool
		// split checks that the table needed more than one Chunk.
		split bool
		// header checks that continuation Chunks carry the table's header.
		header bool
		// rowStarts checks that every Chunk starts at a table row.
		rowStarts bool
	}{
		{
			name:  "a table under the size stays whole",
			text:  "Mở đầu.\n\n" + table + "\n\nKết thúc.",
			cfg:   recursiveCfg(400, 0),
			whole: true,
		},
		{
			name:      "a table over the size splits at rows and repeats the header",
			text:      rowsTable(30),
			cfg:       recursiveCfg(200, 0),
			split:     true,
			header:    true,
			rowStarts: true,
		},
		{
			name:      "a table without a delimiter row splits at rows and carries nothing",
			text:      strings.TrimSuffix(strings.Repeat("| một | hai |\n", 40), "\n"),
			cfg:       recursiveCfg(100, 0),
			split:     true,
			rowStarts: true,
		},
		{
			name:   "a row over its budget is cut and its continuations carry the header",
			text:   "| a | b |\n|---|---|\n|" + strings.Repeat("x", 300) + "|",
			cfg:    recursiveCfg(200, 0),
			split:  true,
			header: true,
		},
		{
			name:   "a row over the header budget is cut even when it fits the chunk size",
			text:   "| a | b |\n|---|---|\n|" + strings.Repeat("x", 188) + "|",
			cfg:    recursiveCfg(200, 0),
			split:  true,
			header: true,
		},
		{
			name:      "a token limit budgets the header with the rows",
			text:      rowsTable(40),
			cfg:       SplitterConfig{TokenLimit: 100, Languages: []string{LangEnglish}, Strategy: StrategyRecursive},
			split:     true,
			header:    true,
			rowStarts: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ensureDefaults(tt.cfg, tt.text)
			chunks := Split(tt.text, tt.cfg)
			assertPositions(t, tt.text, chunks)

			if v := ValidateChunks(tt.text, chunks, cfg); !v.OK {
				t.Fatalf("recursive output rejected: %s", v.Reason)
			}
			if tt.whole {
				assertWhole(t, tt.text, table, chunks)
			}
			if tt.split && len(chunks) < 2 {
				t.Fatalf("got %d chunks, want the table split", len(chunks))
			}

			header := tableHeaderOf(t, tt.text)
			continuations := 0
			for i, c := range chunks {
				if n := utf8.RuneCountInString(c.EmbeddingContent()); n > cfg.ChunkSize {
					t.Errorf("chunk %d embeds %d runes, size is %d", i, n, cfg.ChunkSize)
				}
				if tt.rowStarts && !strings.HasPrefix(c.Content, "|") {
					t.Errorf("chunk %d starts at %q, not at a table row", i, first(c.Content, 20))
				}
				if c.ContextHeader == "" {
					continue
				}
				continuations++
				if !tt.header {
					t.Errorf("chunk %d carries ContextHeader %q, want none", i, c.ContextHeader)
					continue
				}
				if c.ContextHeader != header {
					t.Errorf("chunk %d ContextHeader = %q, want the table header %q", i, c.ContextHeader, header)
				}
				if !strings.HasPrefix(c.EmbeddingContent(), header) {
					t.Errorf("chunk %d embeds %q, want it to start with the header", i, first(c.EmbeddingContent(), 40))
				}
			}
			if tt.header && continuations == 0 {
				t.Fatal("no continuation chunk carries the table header")
			}
			if !tt.header && continuations != 0 {
				t.Fatalf("%d chunks carry a header, want none", continuations)
			}
			if got := restore(t, tt.text, chunks); got != tt.text {
				t.Fatal("chunks do not restore the text")
			}
		})
	}
}

// first returns at most n runes of s, for an error message.
func first(s string, n int) string {
	return string([]rune(s)[:min(n, utf8.RuneCountInString(s))])
}

func TestStructuredTiersPrefixTheBreadcrumbToTheTableHeader(t *testing.T) {
	large := rowsTable(30)
	header := tableHeaderOf(t, large)
	tests := []struct {
		name       string
		text       string
		breadcrumb string
		split      func(string, SplitterConfig) []Chunk
	}{
		{
			name:       "the heading tier",
			text:       "# Biểu thuế\n\n" + large,
			breadcrumb: "Biểu thuế",
			split:      splitHeading,
		},
		{
			name:       "the heuristic tier",
			text:       "Điều 1. Phạm vi\n" + prose(1, 3) + "\n\n" + large + "\n\nĐiều 2. Đối tượng\n" + prose(1, 3),
			breadcrumb: "Điều 1. Phạm vi",
			split:      splitHeuristic,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ensureDefaults(SplitterConfig{ChunkSize: 200, ChunkOverlap: 0}, tt.text)
			chunks := tt.split(tt.text, cfg)
			assertPositions(t, tt.text, chunks)
			if v := ValidateChunks(tt.text, chunks, cfg); !v.OK {
				t.Fatalf("tier output rejected: %s", v.Reason)
			}

			want := tt.breadcrumb + "\n\n" + header
			found := 0
			for i, c := range chunks {
				if n := utf8.RuneCountInString(c.EmbeddingContent()); n > cfg.ChunkSize {
					t.Errorf("chunk %d embeds %d runes, size is %d", i, n, cfg.ChunkSize)
				}
				if !strings.HasSuffix(c.ContextHeader, header) {
					continue
				}
				found++
				if c.ContextHeader != want {
					t.Errorf("chunk %d ContextHeader = %q, want breadcrumb and header %q", i, c.ContextHeader, want)
				}
				if !strings.HasPrefix(c.EmbeddingContent(), want) {
					t.Errorf("chunk %d embeds %q, want it to start with the breadcrumb and header", i, first(c.EmbeddingContent(), 40))
				}
				if !strings.HasPrefix(c.Content, "|") {
					t.Errorf("chunk %d starts at %q, not at a table row", i, first(c.Content, 20))
				}
			}
			if found == 0 {
				t.Fatal("no continuation chunk carries the breadcrumb and the table header")
			}
		})
	}
}
