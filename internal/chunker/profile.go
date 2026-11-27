package chunker

import "unicode/utf8"

// DocProfile is a cheap summary of a Document's structure, used to decide
// whether its headings or heuristic sections are worth splitting at. It is
// also reported in Diagnostics, so a debug view can show why a chain was
// picked.
type DocProfile struct {
	// Runes is the Document's length in runes.
	Runes int `json:"runes"`
	// HeadingsByLevel counts ATX headings outside protected spans: index 0
	// holds level 1 ("#"), index 5 level 6 ("######").
	HeadingsByLevel [6]int `json:"headings_by_level"`
	// HeadedSections is the number of sections that start with a heading and
	// hold body text: the sections the heading tier would emit Chunks for.
	HeadedSections int `json:"headed_sections"`
	// AverageHeadedSectionRunes is the average size of those sections in
	// runes, the density measure SelectTiers uses.
	AverageHeadedSectionRunes int `json:"average_headed_section_runes"`
	// HeuristicSections is the number of sections the heuristic tier would
	// emit Chunks for: every bounded section, the preamble excluded.
	HeuristicSections int `json:"heuristic_sections"`
	// AverageHeuristicSectionRunes is their average size in runes, the
	// density measure SelectTiers uses for the heuristic tier.
	AverageHeuristicSectionRunes int `json:"average_heuristic_section_runes"`
	// LegalArticles counts Điều lines ("Điều 1."), the signal that puts the
	// heuristic tier in legal mode. It is reported so a debug view can show
	// why that mode was picked.
	LegalArticles int `json:"legal_articles"`
}

// HeadingCount is the total number of ATX headings in the Document.
func (p DocProfile) HeadingCount() int {
	total := 0
	for _, n := range p.HeadingsByLevel {
		total += n
	}
	return total
}

const (
	// minStructuredSections is the fewest bounded sections with body a
	// Document needs before splitting at them is worth a tier. A title with
	// one or two sections is not a structure worth its own tier: the
	// recursive tier already keeps it together.
	minStructuredSections = 3
	// minAverageSectionFraction divides ChunkSize to give the least average
	// size of a structured section that makes its tier worth a pass. The
	// validator rejects a Document as fragmented when no Chunk reaches a
	// quarter of the target size, so a Document averaging less is almost
	// certainly one the tier would lose. The floor of tinyChunkRunes is the
	// same signal when ChunkSize is small and the validator's tiny-Chunk
	// rule bites first.
	minAverageSectionFraction = 4
)

// SelectTiers returns the tier chain for a profiled Document: the heading
// tier then the recursive one when there are enough headed sections with
// body text, otherwise the heuristic tier then recursive when its sections
// are structured well enough, and recursive alone when neither is. Long
// sections are no reason to skip a structured tier: both split them inside
// their breadcrumb. cfg must have had its defaults applied; Split passes the
// config ensureDefaults returned.
func SelectTiers(p DocProfile, cfg SplitterConfig) []StrategyTier {
	lower := max(tinyChunkRunes, cfg.ChunkSize/minAverageSectionFraction)
	if p.HeadedSections >= minStructuredSections && p.AverageHeadedSectionRunes >= lower {
		return []StrategyTier{TierHeading, TierRecursive}
	}
	if p.HeuristicSections >= minStructuredSections && p.AverageHeuristicSectionRunes >= lower {
		return []StrategyTier{TierHeuristic, TierRecursive}
	}
	return []StrategyTier{TierRecursive}
}

// ProfileDocument scans text once per detector and returns its DocProfile.
// It counts the sections each tier's scanner returns, so the profile measures
// what that tier would emit.
func ProfileDocument(text string) DocProfile {
	p := DocProfile{Runes: utf8.RuneCountInString(text)}
	hs := headings(text)
	for _, h := range hs {
		p.HeadingsByLevel[h.level-1]++
	}
	p.HeadedSections, p.AverageHeadedSectionRunes = boundedSectionStats(text, hs)

	heuristic, legalArticles := scanHeuristic(text)
	p.LegalArticles = legalArticles
	p.HeuristicSections, p.AverageHeuristicSectionRunes = boundedSectionStats(text, heuristic)
	return p
}

// boundedSectionStats returns how many of the sections hs produce hold body
// text, and their average size in runes. The preamble is excluded: it is not
// a section either structured tier chose to split at.
func boundedSectionStats(text string, hs []heading) (count, avgRunes int) {
	total := 0
	for _, sec := range sectionsFor(text, hs) {
		if !sec.bounded {
			continue
		}
		count++
		total += utf8.RuneCountInString(text[sec.start:sec.end])
	}
	if count > 0 {
		avgRunes = total / count
	}
	return count, avgRunes
}
