package chunker

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// assertParentChild checks the parent-child invariants: children are exactly
// Split's Chunks, parents wrap consecutive children in document order, and
// adjacent parents share at most the configured overlap.
func assertParentChild(t *testing.T, text string, cfg SplitterConfig, result ParentChildResult) {
	t.Helper()
	effective := ensureDefaults(cfg, text)
	children := Split(text, cfg)
	runes := []rune(text)

	if len(result.Children) != len(children) {
		t.Fatalf("got %d children, want Split's %d", len(result.Children), len(children))
	}
	for i := range children {
		if result.Children[i].Chunk != children[i] {
			t.Errorf("child %d = %+v, want Split's %+v", i, result.Children[i].Chunk, children[i])
		}
		if p := result.Children[i].Parent; p < 0 || p >= len(result.Parents) {
			t.Fatalf("child %d has Parent %d, want 0..%d", i, p, len(result.Parents)-1)
		}
	}
	if len(result.Parents) == 0 {
		t.Fatal("no parents")
	}
	if len(result.Parents) > len(result.Children) {
		t.Fatalf("got %d parents for %d children", len(result.Parents), len(result.Children))
	}

	last := -1
	for i, p := range result.Parents {
		if p.Seq != i {
			t.Errorf("parent %d has Seq %d", i, p.Seq)
		}
		if p.Start < 0 || p.End > len(runes) || p.Start >= p.End {
			t.Fatalf("parent %d: bad offsets [%d,%d) for %d runes", i, p.Start, p.End, len(runes))
		}
		if got := string(runes[p.Start:p.End]); got != p.Content {
			t.Errorf("parent %d: runes[%d:%d] = %q, Content = %q", i, p.Start, p.End, got, p.Content)
		}
		if i > 0 {
			prev := result.Parents[i-1]
			if p.Start < prev.Start || p.End <= prev.End {
				t.Errorf("parent %d [%d,%d) does not move past parent %d [%d,%d)", i, p.Start, p.End, i-1, prev.Start, prev.End)
			}
			if overlap := prev.End - p.Start; overlap > effective.ChunkOverlap {
				t.Errorf("parents %d and %d share %d runes, overlap is %d", i-1, i, overlap, effective.ChunkOverlap)
			}
		}

		first := last + 1
		for last+1 < len(result.Children) && result.Children[last+1].Parent == i {
			last++
		}
		if last < first {
			t.Fatalf("parent %d has no children", i)
		}
		if got := result.Children[first].ContextHeader; got != p.ContextHeader {
			t.Errorf("parent %d ContextHeader = %q, want its first child's %q", i, p.ContextHeader, got)
		}
		for j := first; j <= last; j++ {
			c := result.Children[j]
			if c.Start < p.Start || c.End > p.End {
				t.Errorf("child %d [%d,%d) lies outside parent %d [%d,%d)", j, c.Start, c.End, i, p.Start, p.End)
			}
		}
		if n := p.End - p.Start; effective.ParentChunkSize > 0 && n > effective.ParentChunkSize && last > first {
			t.Errorf("parent %d spans %d runes with %d children, over %d", i, n, last-first+1, effective.ParentChunkSize)
		}
	}
	if last != len(result.Children)-1 {
		t.Fatalf("%d children are not assigned to a parent", len(result.Children)-1-last)
	}
}

func childrenOf(result ParentChildResult, parent int) []ChildChunk {
	var out []ChildChunk
	for _, c := range result.Children {
		if c.Parent == parent {
			out = append(out, c)
		}
	}
	return out
}

func TestSplitParentChild(t *testing.T) {
	headingDoc := "# Chương I\n\n" + prose(2, 4) +
		"\n\n## Điều 1. Phạm vi\n\n" + prose(2, 4) +
		"\n\n## Điều 2. Đối tượng\n\n" + prose(2, 4)
	legal := loadLegalSample(t)
	shortSections := shortSectionDocument()
	fence := "```go\n" + strings.Repeat("fmt.Println(\"thuế\")\n", 20) + "```"
	fencedDoc := prose(1, 2) + "\n\n" + fence + "\n\n" + prose(1, 2)

	offCfg := SplitterConfig{ChunkSize: 120, ChunkOverlap: 0, Strategy: StrategyAuto}
	legalCfg := SplitterConfig{ChunkSize: 300, ChunkOverlap: 60, Strategy: StrategyAuto, ParentChunkSize: 1500}
	shortCfg := SplitterConfig{ChunkSize: 300, ChunkOverlap: 0, Strategy: StrategyAuto, ParentChunkSize: 600}
	fenceCfg := SplitterConfig{ChunkSize: 100, ChunkOverlap: 0, Strategy: StrategyRecursive, ParentChunkSize: 200}
	overlapCfg := SplitterConfig{ChunkSize: 200, ChunkOverlap: 80, Strategy: StrategyRecursive, ParentChunkSize: 401}

	tests := []struct {
		name  string
		text  string
		cfg   SplitterConfig
		check func(t *testing.T, result ParentChildResult)
	}{
		{
			name: "an unset parent size keeps one parent per child",
			text: headingDoc,
			cfg:  offCfg,
			check: func(t *testing.T, result ParentChildResult) {
				t.Helper()
				children := Split(headingDoc, offCfg)
				if len(result.Parents) != len(children) {
					t.Fatalf("got %d parents, want one per child", len(result.Parents))
				}
				for i := range children {
					if result.Parents[i] != children[i] {
						t.Errorf("parent %d = %+v, want child %+v", i, result.Parents[i], children[i])
					}
					if result.Children[i].Parent != i {
						t.Errorf("child %d has Parent %d", i, result.Children[i].Parent)
					}
				}
			},
		},
		{
			name: "a legal document packs several articles into a parent",
			text: legal,
			cfg:  legalCfg,
			check: func(t *testing.T, result ParentChildResult) {
				t.Helper()
				if len(result.Parents) >= len(result.Children) {
					t.Fatalf("got %d parents and %d children, want parents to span children", len(result.Parents), len(result.Children))
				}
				multi := 0
				for _, p := range result.Parents {
					articles := 0
					for _, line := range strings.Split(p.Content, "\n") {
						if strings.HasPrefix(line, "Điều ") {
							articles++
						}
					}
					if articles >= 2 {
						multi++
					}
				}
				if multi == 0 {
					t.Fatal("no parent holds children of several articles")
				}
			},
		},
		{
			name: "short Markdown sections share a parent",
			text: shortSections,
			cfg:  shortCfg,
			check: func(t *testing.T, result ParentChildResult) {
				t.Helper()
				if len(result.Parents) >= len(result.Children) {
					t.Fatalf("got %d parents and %d children, want parents to span sections", len(result.Parents), len(result.Children))
				}
				multi := 0
				for _, p := range result.Parents {
					if strings.Count(p.Content, "## Phần ") >= 2 {
						multi++
					}
				}
				if multi == 0 {
					t.Fatal("no parent holds several Markdown sections")
				}
			},
		},
		{
			name: "a protected-span child over the parent size is its own parent",
			text: fencedDoc,
			cfg:  fenceCfg,
			check: func(t *testing.T, result ParentChildResult) {
				t.Helper()
				found := false
				for i, p := range result.Parents {
					if !strings.Contains(p.Content, "fmt.Println") {
						continue
					}
					found = true
					if n := utf8.RuneCountInString(p.Content); n <= fenceCfg.ParentChunkSize {
						t.Errorf("the fence parent has %d runes, want over %d", n, fenceCfg.ParentChunkSize)
					}
					kids := childrenOf(result, i)
					if len(kids) != 1 || kids[0].Start != p.Start || kids[0].End != p.End {
						t.Errorf("the fence parent holds %d children, want the fence child alone", len(kids))
					}
				}
				if !found {
					t.Fatal("no parent holds the fenced code child")
				}
			},
		},
		{
			name: "adjacent parents share at most the child overlap",
			text: prose(8, 4),
			cfg:  overlapCfg,
			check: func(t *testing.T, result ParentChildResult) {
				t.Helper()
				if len(result.Parents) < 2 {
					t.Fatalf("got %d parents, want several", len(result.Parents))
				}
				overlapped := 0
				for i := 1; i < len(result.Parents); i++ {
					if overlap := result.Parents[i-1].End - result.Parents[i].Start; overlap > 0 {
						overlapped++
						if overlap > overlapCfg.ChunkOverlap {
							t.Errorf("parents %d and %d share %d runes, overlap is %d", i-1, i, overlap, overlapCfg.ChunkOverlap)
						}
					}
				}
				if overlapped == 0 {
					t.Fatal("no adjacent parents overlap")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SplitParentChild(tt.text, tt.cfg)
			assertParentChild(t, tt.text, tt.cfg, result)
			tt.check(t, result)
		})
	}
}

// shortSectionDocument is six short Markdown sections, each a child of its
// own at ChunkSize 300.
func shortSectionDocument() string {
	var b strings.Builder
	for i := range 6 {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## Phần %d\n\n", i)
		b.WriteString(prose(1, 2))
	}
	return b.String()
}

func TestParentChunkSizeIsAtLeastTwiceTheChildSize(t *testing.T) {
	tests := []struct {
		size, parent, want int
	}{
		{120, 0, 0},
		{120, -5, 0},
		{120, 100, 240},
		{120, 240, 240},
		{120, 500, 500},
	}
	for _, tt := range tests {
		got := NormalizeSplitterConfig(SplitterConfig{ChunkSize: tt.size, ParentChunkSize: tt.parent})
		if got.ParentChunkSize != tt.want {
			t.Errorf("ChunkSize %d, ParentChunkSize %d normalized to %d, want %d", tt.size, tt.parent, got.ParentChunkSize, tt.want)
		}
	}

	text := prose(6, 4)
	low := SplitParentChild(text, SplitterConfig{ChunkSize: 100, Strategy: StrategyRecursive, ParentChunkSize: 150})
	raised := SplitParentChild(text, SplitterConfig{ChunkSize: 100, Strategy: StrategyRecursive, ParentChunkSize: 200})
	if !slices.Equal(low.Parents, raised.Parents) || !slices.Equal(low.Children, raised.Children) {
		t.Fatal("a parent size below twice the child size did not split like the raised one")
	}
}

func TestSplitParentChildDiagnosticsAreTheChildSplit(t *testing.T) {
	text := loadLegalSample(t)
	cfg := SplitterConfig{ChunkSize: 300, ChunkOverlap: 60, Strategy: StrategyAuto, ParentChunkSize: 1500}
	_, got := SplitParentChildWithDiagnostics(text, cfg)
	_, want := SplitWithDiagnostics(text, cfg)

	if got.SelectedTier != want.SelectedTier {
		t.Errorf("selected %q, want the child split's %q", got.SelectedTier, want.SelectedTier)
	}
	if !slices.Equal(got.TierChain, want.TierChain) {
		t.Errorf("chain = %v, want %v", got.TierChain, want.TierChain)
	}
	if !slices.Equal(got.Rejected, want.Rejected) {
		t.Errorf("rejected = %v, want %v", got.Rejected, want.Rejected)
	}
	if (got.Profile == nil) != (want.Profile == nil) || (got.Profile != nil && *got.Profile != *want.Profile) {
		t.Errorf("profile = %+v, want %+v", got.Profile, want.Profile)
	}
}
