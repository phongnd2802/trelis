package chunker

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// withTier registers a tier for one test.
func withTier(t *testing.T, tier StrategyTier, split func(string, SplitterConfig) []Chunk) {
	t.Helper()
	tierSplitters[tier] = split
	t.Cleanup(func() { delete(tierSplitters, tier) })
}

// chunkAt makes a Chunk of text's runes [start,end).
func chunkAt(text string, seq, start, end int) Chunk {
	return Chunk{Content: string([]rune(text)[start:end]), Seq: seq, Start: start, End: end}
}

// byLines makes one Chunk per line, whatever is protected.
func byLines(text string, _ SplitterConfig) []Chunk {
	var out []Chunk
	start := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		n := utf8.RuneCountInString(line)
		if strings.TrimSpace(line) != "" {
			out = append(out, chunkAt(text, len(out), start, start+n))
		}
		start += n
	}
	return out
}

func TestTierFallThrough(t *testing.T) {
	withTier(t, "lines", byLines)
	withTier(t, "empty", func(string, SplitterConfig) []Chunk { return nil })
	withTier(t, "whole", func(text string, _ SplitterConfig) []Chunk {
		return []Chunk{chunkAt(text, 0, 0, utf8.RuneCountInString(text))}
	})
	withTier(t, "lying", func(text string, _ SplitterConfig) []Chunk {
		return []Chunk{{Content: "không phải văn bản gốc", Start: 0, End: 5}}
	})

	tableDoc := prose(2, 3) + "\n\n" + table + "\n\n" + prose(2, 3)
	tests := []struct {
		name     string
		text     string
		chain    []StrategyTier
		size     int
		selected StrategyTier
		rejected []TierRejection
	}{
		{
			name:     "an accepted first tier is used",
			text:     prose(6, 3),
			chain:    []StrategyTier{"lines", TierRecursive},
			size:     200,
			selected: "lines",
		},
		{
			name:     "a tier that splits a table falls through",
			text:     tableDoc,
			chain:    []StrategyTier{"lines", TierRecursive},
			size:     300,
			selected: TierRecursive,
			rejected: []TierRejection{{"lines", "cuts a protected span"}},
		},
		{
			name:     "a tier with no chunks falls through",
			text:     prose(3, 3),
			chain:    []StrategyTier{"empty", TierRecursive},
			size:     200,
			selected: TierRecursive,
			rejected: []TierRejection{{"empty", "no chunks"}},
		},
		{
			name:     "a tier that does not split a large document falls through",
			text:     prose(10, 3),
			chain:    []StrategyTier{"whole", TierRecursive},
			size:     200,
			selected: TierRecursive,
			rejected: []TierRejection{{"whole", "one chunk for a document"}},
		},
		{
			name:     "a tier that breaks the position invariant falls through",
			text:     prose(2, 2),
			chain:    []StrategyTier{"lying", TierRecursive},
			size:     200,
			selected: TierRecursive,
			rejected: []TierRejection{{"lying", "is not the source text"}},
		},
		{
			name:     "tiers are tried in order",
			text:     tableDoc,
			chain:    []StrategyTier{"empty", "lines", TierRecursive},
			size:     300,
			selected: TierRecursive,
			rejected: []TierRejection{{"empty", "no chunks"}, {"lines", "protected span"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ensureDefaults(SplitterConfig{ChunkSize: tt.size, ChunkOverlap: 0}, tt.text)
			chunks, diag := runChain(tt.text, cfg, tt.chain)

			if diag.SelectedTier != tt.selected {
				t.Fatalf("selected %q, want %q (rejected %v)", diag.SelectedTier, tt.selected, diag.Rejected)
			}
			if !slices.Equal(diag.TierChain, tt.chain) {
				t.Fatalf("chain = %v, want %v", diag.TierChain, tt.chain)
			}
			if len(diag.Rejected) != len(tt.rejected) {
				t.Fatalf("rejected = %v, want %v", diag.Rejected, tt.rejected)
			}
			for i, r := range diag.Rejected {
				if r.Tier != tt.rejected[i].Tier || !strings.Contains(r.Reason, tt.rejected[i].Reason) {
					t.Fatalf("rejection %d = %v, want tier %q with a reason about %q", i, r, tt.rejected[i].Tier, tt.rejected[i].Reason)
				}
			}
			assertPositions(t, tt.text, chunks)
			if strings.Contains(tt.text, table) {
				assertWhole(t, tt.text, table, chunks)
			}
		})
	}
}

func TestTheLastTierIsReturnedEvenWhenRejected(t *testing.T) {
	// A 20-rune size makes every Chunk tiny, which the validator rejects.
	text := prose(4, 4)
	chunks, diag := SplitWithDiagnostics(text, recursiveCfg(20, 0))
	if len(chunks) == 0 {
		t.Fatal("got no chunks")
	}
	if diag.SelectedTier != TierRecursive {
		t.Fatalf("selected %q, want recursive", diag.SelectedTier)
	}
	if len(diag.Rejected) != 1 || !strings.Contains(diag.Rejected[0].Reason, "fragmented") {
		t.Fatalf("rejected = %v, want the recursive tier rejected for tiny chunks", diag.Rejected)
	}
	assertPositions(t, text, chunks)
}

func TestLargeProtectedSpansAreNotRejected(t *testing.T) {
	var rows strings.Builder
	rows.WriteString("| Bậc | Thuế suất |\n|---|---|\n")
	for i := range 100 {
		fmt.Fprintf(&rows, "| %d | %d%% |\n", i, i)
	}
	text := "Mở đầu.\n\n" + rows.String()
	tests := []struct {
		name string
		cfg  SplitterConfig
	}{
		{"kept whole over twice the size", SplitterConfig{Strategy: StrategyRecursive}},
		{"cut at the token limit", SplitterConfig{TokenLimit: 300, Languages: []string{LangEnglish}, Strategy: StrategyRecursive}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks, diag := SplitWithDiagnostics(text, tt.cfg)
			if len(diag.Rejected) != 0 {
				t.Fatalf("recursive output rejected: %v", diag.Rejected)
			}
			assertPositions(t, text, chunks)
		})
	}
}

func TestSplitAndDiagnosticsAgree(t *testing.T) {
	text := prose(10, 4) + "\n\n" + table
	for _, strategy := range []string{"", StrategyAuto, StrategyRecursive, "no-such-strategy"} {
		cfg := SplitterConfig{Strategy: strategy}
		withDiag, diag := SplitWithDiagnostics(text, cfg)
		if got := Split(text, cfg); !slices.Equal(got, withDiag) {
			t.Fatalf("strategy %q: Split and SplitWithDiagnostics disagree", strategy)
		}
		if diag.SelectedTier != TierRecursive || len(diag.Rejected) != 0 {
			t.Fatalf("strategy %q: diagnostics = %+v, want recursive accepted", strategy, diag)
		}
	}
}

func TestAutoTierChains(t *testing.T) {
	var dense strings.Builder
	dense.WriteString("# Tài liệu\n\n")
	dense.WriteString(prose(1, 3))
	dense.WriteString("\n\n")
	for i := range 5 {
		fmt.Fprintf(&dense, "## Phần %d\n\n", i)
		dense.WriteString(prose(1, 3))
		dense.WriteString("\n\n")
	}
	fragmented := "# Dài\n\n" + prose(2, 3) + "\n\n" + strings.Repeat("# Ngắn\n\nx\n\n", 4)
	sparse := "# Mở đầu\n\n" + prose(20, 4)

	// Fragmentation is the only validator rejection the real heading tier can
	// reach through StrategyAuto: the profiler counts at least three headed
	// sections with body (so "no chunks" and "one chunk" are out), heading
	// boundaries sit outside protected spans, and the lower bound keeps the
	// longest Chunk over a quarter of the target.
	tests := []struct {
		name     string
		text     string
		cfg      SplitterConfig
		chain    []StrategyTier
		selected StrategyTier
		rejected []TierRejection
	}{
		{
			name:     "the heading tier is picked and accepted",
			text:     dense.String(),
			cfg:      SplitterConfig{ChunkSize: 200, Strategy: StrategyAuto},
			chain:    []StrategyTier{TierHeading, TierRecursive},
			selected: TierHeading,
		},
		{
			name:     "the heading tier fragments and falls back",
			text:     fragmented,
			cfg:      SplitterConfig{ChunkSize: 100, Strategy: StrategyAuto},
			chain:    []StrategyTier{TierHeading, TierRecursive},
			selected: TierRecursive,
			rejected: []TierRejection{{Tier: TierHeading, Reason: "fragmented"}},
		},
		{
			name:     "sparse headings go straight to recursive",
			text:     sparse,
			cfg:      SplitterConfig{Strategy: StrategyAuto},
			chain:    []StrategyTier{TierRecursive},
			selected: TierRecursive,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks, diag := SplitWithDiagnostics(tt.text, tt.cfg)
			if !slices.Equal(diag.TierChain, tt.chain) {
				t.Fatalf("chain = %v, want %v", diag.TierChain, tt.chain)
			}
			if diag.SelectedTier != tt.selected {
				t.Fatalf("selected %q, want %q (rejected %v)", diag.SelectedTier, tt.selected, diag.Rejected)
			}
			if len(diag.Rejected) != len(tt.rejected) {
				t.Fatalf("rejected = %v, want %v", diag.Rejected, tt.rejected)
			}
			for i, want := range tt.rejected {
				got := diag.Rejected[i]
				if got.Tier != want.Tier || !strings.Contains(got.Reason, want.Reason) {
					t.Fatalf("rejection %d = %v, want tier %q with a reason about %q", i, got, want.Tier, want.Reason)
				}
			}
			if diag.Profile == nil {
				t.Fatal("the auto strategy returned no profile")
			}
			assertPositions(t, tt.text, chunks)
		})
	}
}

func TestRecursiveNeverProfiles(t *testing.T) {
	text := strings.Repeat("# Phần\n\nNội dung.\n\n", 6)

	_, diag := SplitWithDiagnostics(text, SplitterConfig{Strategy: StrategyRecursive})
	if diag.Profile != nil {
		t.Fatalf("profile = %+v, want nil for an explicit strategy", diag.Profile)
	}
	if !slices.Equal(diag.TierChain, []StrategyTier{TierRecursive}) {
		t.Fatalf("chain = %v, want recursive alone", diag.TierChain)
	}
}

func TestDiagnosticsForEmptyText(t *testing.T) {
	chunks, diag := SplitWithDiagnostics("", DefaultConfig())
	if chunks != nil || diag == nil || diag.SelectedTier != TierRecursive {
		t.Fatalf("chunks = %v, diag = %+v", chunks, diag)
	}
}

func TestValidateChunks(t *testing.T) {
	long := strings.Repeat("a", 1000)
	evenly := func(text string, size int) []Chunk {
		var out []Chunk
		n := utf8.RuneCountInString(text)
		for start := 0; start < n; start += size {
			out = append(out, chunkAt(text, len(out), start, min(start+size, n)))
		}
		return out
	}
	tests := []struct {
		name   string
		text   string
		chunks []Chunk
		size   int
		reason string // empty means OK
	}{
		{"empty", long, nil, 100, "no chunks"},
		{"single chunk for a large document", long, []Chunk{chunkAt(long, 0, 0, 1000)}, 100, "one chunk for a document"},
		{"reasonable output", long, evenly(long, 100), 100, ""},
		{"a chunk over twice the size", long, append(evenly(long[:700], 100), chunkAt(long, 7, 700, 1000)), 100, "over twice the target size"},
		{"a tiny tail is fine", long[:960], evenly(long[:960], 100), 100, ""},
		{"fragmented output", long, evenly(long, 10), 100, "fragmented"},
		{"all chunks far below the size", strings.Repeat("a", 600), evenly(strings.Repeat("a", 600), 60), 400, "quarter of the target size"},
		{"one chunk with offsets past the text", long, []Chunk{{Content: long, Start: 0, End: 5000}}, 100, "one chunk for a document"},
		{"content not at its offsets", "xin chào", []Chunk{{Content: "chào", Start: 0, End: 4}}, 100, "is not the source text"},
		{"a cut table", "a\n" + table, byLines("a\n"+table, SplitterConfig{}), 1000, "cuts a protected span"},
		{"a lone protected span over twice the size", "Mở đầu.\n\n" + table, []Chunk{chunkAt("Mở đầu.\n\n"+table, 0, 0, 9), chunkAt("Mở đầu.\n\n"+table, 1, 9, 9+utf8.RuneCountInString(table))}, 60, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := ValidateChunks(tt.text, tt.chunks, ensureDefaults(SplitterConfig{ChunkSize: tt.size, ChunkOverlap: 0}, tt.text))
			if tt.reason == "" {
				if !v.OK {
					t.Fatalf("rejected: %s", v.Reason)
				}
				return
			}
			if v.OK || !strings.Contains(v.Reason, tt.reason) {
				t.Fatalf("verdict = %+v, want rejected for %q", v, tt.reason)
			}
		})
	}
}

func FuzzSplitKeepsPositionsAndText(f *testing.F) {
	f.Add("Mở đầu.\n\n"+table+"\n\n"+prose(2, 3), 60, 12)
	f.Add("```\na\n\nb\n```\n$$\nx\n\n$$\n| a |\n| b |", 8, 3)
	f.Add("~~~\n```\n~~~ x\n`a` [b](c) ![d](e)", 5, 2)
	f.Add(strings.Repeat("đ. ", 50)+"\r\n\r\nx", 7, 3)
	f.Fuzz(func(t *testing.T, text string, size, overlap int) {
		if size < 1 || size > 500 || overlap < 0 || !utf8.ValidString(text) {
			t.Skip()
		}
		chunks := Split(text, recursiveCfg(size, overlap))
		if strings.TrimSpace(text) != "" && len(chunks) == 0 {
			t.Fatal("no chunks for non-blank text")
		}
		assertPositions(t, text, chunks)
		for i := 1; i < len(chunks); i++ {
			if chunks[i].Start < chunks[i-1].Start || chunks[i].End <= chunks[i-1].End {
				t.Fatalf("chunk %d [%d,%d) does not move past chunk %d [%d,%d)", i, chunks[i].Start, chunks[i].End, i-1, chunks[i-1].Start, chunks[i-1].End)
			}
		}
		if got := restore(t, text, chunks); got != text {
			t.Fatal("chunks do not restore the text")
		}
		protected := toRuneSpans(text, protectedSpans(text))
		for i, c := range chunks {
			if utf8.RuneCountInString(c.Content) > size && !isProtectedRun(protected, c) {
				t.Fatalf("chunk %d has %d runes, size is %d, and is not a protected span", i, utf8.RuneCountInString(c.Content), size)
			}
		}
	})
}

// isProtectedRun reports whether c holds part of a protected span, the only
// thing that may take a Chunk over the size.
func isProtectedRun(protected []span, c Chunk) bool {
	for _, s := range protected {
		if s.start < c.End && s.end > c.Start {
			return true
		}
	}
	return false
}
