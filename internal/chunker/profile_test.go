package chunker

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProfileDocument(t *testing.T) {
	text := "# Một\n\nThân một.\n\n## Hai\n\nThân hai.\n\n# Ba\n\nThân ba."
	p := ProfileDocument(text)

	if want := utf8.RuneCountInString(text); p.Runes != want {
		t.Errorf("Runes = %d, want %d", p.Runes, want)
	}
	wantLevels := [6]int{2, 1, 0, 0, 0, 0}
	if p.HeadingsByLevel != wantLevels {
		t.Errorf("HeadingsByLevel = %v, want %v", p.HeadingsByLevel, wantLevels)
	}
	if got := p.HeadingCount(); got != 3 {
		t.Errorf("HeadingCount = %d, want 3", got)
	}
	if got := p.HeadedSections; got != 3 {
		t.Errorf("HeadedSections = %d, want 3", got)
	}
	if want := p.Runes / 3; p.AverageHeadedSectionRunes != want {
		t.Errorf("AverageHeadedSectionRunes = %d, want %d", p.AverageHeadedSectionRunes, want)
	}
}

func TestProfileDocumentCountsOnlyHeadedSectionsWithBody(t *testing.T) {
	tests := []struct {
		name           string
		text           string
		headings       int
		headedSections int
	}{
		{"a preamble is not a headed section", "Mở đầu.\n\n# A\n\nbody", 1, 1},
		{"a body-less heading is not a section", "# A\n\n## B\n\nbody", 2, 1},
		{"headings inside code are not headings", "```\n# a\n# b\n# c\n```\n\nbody", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ProfileDocument(tt.text)
			if got := p.HeadingCount(); got != tt.headings {
				t.Errorf("HeadingCount = %d, want %d", got, tt.headings)
			}
			if got := p.HeadedSections; got != tt.headedSections {
				t.Errorf("HeadedSections = %d, want %d", got, tt.headedSections)
			}
		})
	}
}

func TestProfileDocumentCountsLegalStructure(t *testing.T) {
	text := loadLegalSample(t)
	p := ProfileDocument(text)

	if p.LegalArticles != 6 {
		t.Errorf("LegalArticles = %d, want the 6 Điều lines", p.LegalArticles)
	}
	if p.HeuristicSections != 6 {
		t.Errorf("HeuristicSections = %d, want the 6 article sections (chapter and mục titles are body-less)", p.HeuristicSections)
	}
	if p.HeadedSections != 0 {
		t.Errorf("HeadedSections = %d, want 0 for a document without ATX headings", p.HeadedSections)
	}
	if lower := max(tinyChunkRunes, DefaultChunkSize/minAverageSectionFraction); p.AverageHeuristicSectionRunes < lower {
		t.Errorf("AverageHeuristicSectionRunes = %d, want at least %d", p.AverageHeuristicSectionRunes, lower)
	}
	if got := SelectTiers(p, SplitterConfig{ChunkSize: DefaultChunkSize}); !slices.Equal(got, []StrategyTier{TierHeuristic, TierRecursive}) {
		t.Errorf("chain = %v, want heuristic then recursive", got)
	}
}

func TestSelectTiers(t *testing.T) {
	tests := []struct {
		name string
		text string
		size int
		want []StrategyTier
	}{
		{
			name: "no headings",
			text: prose(20, 4),
			size: 100,
			want: []StrategyTier{TierRecursive},
		},
		{
			name: "few long sections (the legal case)",
			text: strings.Repeat("# Chương\n\n"+prose(4, 4)+"\n\n", 5),
			size: 100,
			want: []StrategyTier{TierHeading, TierRecursive},
		},
		{
			name: "a lone title in a long document",
			text: "# Mở đầu\n\n" + prose(20, 4),
			size: 100,
			want: []StrategyTier{TierRecursive},
		},
		{
			name: "table-of-contents-like tiny sections",
			text: strings.Repeat("# Mục\n\nx\n\n", 10),
			size: 512,
			want: []StrategyTier{TierRecursive},
		},
		{
			name: "sections below a quarter of the chunk size",
			text: strings.Repeat("# Mục\n\n"+strings.Repeat("Nội dung ngắn. ", 4)+"\n\n", 5),
			size: 512,
			want: []StrategyTier{TierRecursive},
		},
		{
			name: "headings only inside code",
			text: "```\n# a\n# b\n# c\n```\n\n" + prose(2, 3),
			size: 100,
			want: []StrategyTier{TierRecursive},
		},
		{
			name: "dense headings",
			text: strings.Repeat("# Phần\n\n"+prose(1, 2)+"\n\n", 5),
			size: 100,
			want: []StrategyTier{TierHeading, TierRecursive},
		},
		{
			name: "a Vietnamese legal document",
			text: loadLegalSample(t),
			size: DefaultChunkSize,
			want: []StrategyTier{TierHeuristic, TierRecursive},
		},
		{
			name: "uppercase section titles",
			text: uppercaseTitledDocument(),
			size: 200,
			want: []StrategyTier{TierHeuristic, TierRecursive},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SelectTiers(ProfileDocument(tt.text), SplitterConfig{ChunkSize: tt.size})
			if !slices.Equal(got, tt.want) {
				t.Fatalf("chain = %v, want %v", got, tt.want)
			}
		})
	}
}
