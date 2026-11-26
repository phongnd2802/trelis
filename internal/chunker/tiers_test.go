package chunker_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/phongnd2802/trelis/internal/chunker"
)

// tierFunc is a Tier built from a function, for tests.
type tierFunc struct {
	name  string
	split func(in chunker.Input, size chunker.Sizer) []chunker.Chunk
}

func (f tierFunc) Name() string { return f.name }
func (f tierFunc) Split(in chunker.Input, size chunker.Sizer) []chunker.Chunk {
	return f.split(in, size)
}

// byLines is a Tier that makes one Chunk per non-blank line, whatever the
// size cap or protected spans say.
var byLines = tierFunc{"lines", func(in chunker.Input, _ chunker.Sizer) []chunker.Chunk {
	var out []chunker.Chunk
	start := 0
	for _, l := range strings.SplitAfter(in.Text, "\n") {
		if s := strings.TrimSpace(l); s != "" {
			at := start + strings.Index(l, s)
			out = append(out, chunker.Chunk{Content: s, Start: at, End: at + len(s)})
		}
		start += len(l)
	}
	return out
}}

func constant(name string, chunks ...chunker.Chunk) tierFunc {
	return tierFunc{name, func(chunker.Input, chunker.Sizer) []chunker.Chunk { return chunks }}
}

func TestTierFallThrough(t *testing.T) {
	const text = "Mở đầu ngắn.\n\n" + table + "\n\nKết thúc ngắn."
	tableAt := strings.Index(text, table)
	tests := []struct {
		name     string
		text     string // defaults to text
		tiers    []chunker.Tier
		limit    int
		wantTier string
		rejected []string
		reason   string // in the first rejection
	}{
		{
			name:     "a valid first tier is used",
			text:     "Dòng một.\nDòng hai.\nDòng ba.",
			tiers:    []chunker.Tier{byLines},
			limit:    1000,
			wantTier: "lines",
		},
		{
			name:     "a tier that splits a table falls through",
			tiers:    []chunker.Tier{byLines},
			limit:    1000,
			wantTier: "recursive",
			rejected: []string{"lines"},
			reason:   "protected span",
		},
		{
			name:     "a tier with no Chunks falls through",
			tiers:    []chunker.Tier{constant("empty")},
			limit:    1000,
			wantTier: "recursive",
			rejected: []string{"empty"},
			reason:   "no chunks",
		},
		{
			name: "a tier over the size cap falls through",
			tiers: []chunker.Tier{
				constant("whole", chunker.Chunk{Content: strings.TrimSpace(text), Start: 0, End: len(text)}),
			},
			limit:    10,
			wantTier: "recursive",
			rejected: []string{"whole"},
			reason:   "over the cap",
		},
		{
			name: "a tier whose offsets do not match its Content falls through",
			tiers: []chunker.Tier{
				constant("lying", chunker.Chunk{Content: "something else", Start: 0, End: len(text)}),
			},
			limit:    1000,
			wantTier: "recursive",
			rejected: []string{"lying"},
			reason:   "Content is not",
		},
		{
			name: "a tier that drops text falls through",
			tiers: []chunker.Tier{
				constant("lossy", chunker.Chunk{Content: table, Start: tableAt, End: tableAt + len(table)}),
			},
			limit:    1000,
			wantTier: "recursive",
			rejected: []string{"lossy"},
			reason:   "in no chunk",
		},
		{
			name:     "rejections are tried in order",
			tiers:    []chunker.Tier{constant("empty"), byLines, constant("never")},
			limit:    1000,
			wantTier: "recursive",
			rejected: []string{"empty", "lines", "never"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.text
			if in == "" {
				in = text
			}
			c, err := chunker.New(chunker.Config{TokenLimit: tt.limit, CountTokens: words}, chunker.WithTiers(tt.tiers...))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			res := c.Chunk(in)
			if res.Tier != tt.wantTier {
				t.Fatalf("Tier = %q, want %q (rejections %v)", res.Tier, tt.wantTier, res.Rejected)
			}
			var rejected []string
			for _, r := range res.Rejected {
				if r.Reason == nil {
					t.Errorf("rejection of %q has no reason", r.Tier)
				}
				rejected = append(rejected, r.Tier)
			}
			if !slices.Equal(rejected, tt.rejected) {
				t.Fatalf("rejected = %q, want %q", rejected, tt.rejected)
			}
			if tt.reason != "" && !strings.Contains(res.Rejected[0].Reason.Error(), tt.reason) {
				t.Fatalf("rejected because %q, want a reason about %q", res.Rejected[0].Reason, tt.reason)
			}
			assertOffsets(t, in, res.Chunks)
			assertCovers(t, in, res.Chunks)
			if strings.Contains(in, table) {
				assertWhole(t, in, table, res.Chunks)
			}
		})
	}
}

func TestTheFinalTierAlwaysReturnsChunks(t *testing.T) {
	never := func(chunker.Input, []chunker.Chunk, chunker.Sizer) error { return errors.New("never good enough") }
	c, err := chunker.New(chunker.Config{TokenLimit: 20, CountTokens: words},
		chunker.WithTiers(byLines), chunker.WithValidator(never))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := prose(3, 4)
	res := c.Chunk(text)

	if res.Tier != "recursive" {
		t.Fatalf("Tier = %q, want recursive", res.Tier)
	}
	if len(res.Chunks) == 0 {
		t.Fatal("got no chunks")
	}
	if got := len(res.Rejected); got != 2 {
		t.Fatalf("got %d rejections, want both tiers rejected", got)
	}
	assertOffsets(t, text, res.Chunks)
	assertCovers(t, text, res.Chunks)
}

func FuzzRecursiveOutputIsValid(f *testing.F) {
	f.Add("Mở đầu.\n\n"+table+"\n\n"+prose(2, 3), 7)
	f.Add("```\na\n\nb\n```\n$$\nx\n\n$$\n| a |\n| b |", 1)
	f.Add("~~~\n```\n~~~ x\n", 2)
	f.Add(strings.Repeat("đ ", 50), 3)
	f.Fuzz(func(t *testing.T, text string, limit int) {
		if limit < 1 || limit > 200 || !utf8.ValidString(text) {
			t.Skip()
		}
		res := mustNew(t, chunker.Config{TokenLimit: limit}).Chunk(text)
		if len(res.Rejected) != 0 {
			t.Fatalf("recursive output rejected: %v", res.Rejected[0].Reason)
		}
		if strings.TrimSpace(text) != "" && len(res.Chunks) == 0 {
			t.Fatal("no chunks for non-blank text")
		}
	})
}

func TestRecursiveOutputPassesTheDefaultValidator(t *testing.T) {
	texts := []string{
		prose(10, 6),
		"Mở đầu.\n\n" + table + "\n\n" + prose(2, 3),
		"```\n" + prose(3, 3) + "\n```\n" + strings.Repeat("đ", 500),
	}
	for _, text := range texts {
		for _, limit := range []int{3, 15, 100} {
			res := mustNew(t, chunker.Config{TokenLimit: limit, CountTokens: words}).Chunk(text)
			if len(res.Rejected) != 0 {
				t.Errorf("limit %d: recursive output rejected: %v", limit, res.Rejected[0].Reason)
			}
		}
	}
}
