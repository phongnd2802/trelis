package chunker

import "unicode/utf8"

// DocProfile is a cheap summary of a Document's structure, used to decide
// whether its headings are worth splitting at. It is also reported in
// Diagnostics, so a debug view can show why a chain was picked.
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
	// minHeadingsForHeadingTier is the fewest headed sections with body a
	// Document needs before splitting at headings is worth a tier. A title
	// with one or two sections is not a structure worth its own tier: the
	// recursive tier already keeps it together.
	minHeadingsForHeadingTier = 3
	// minAverageSectionFraction divides ChunkSize to give the least average
	// size of a headed section that makes the heading tier worth a pass. The
	// validator rejects a Document as fragmented when no Chunk reaches a
	// quarter of the target size, so a Document averaging less is almost
	// certainly one the heading tier would lose. The floor of tinyChunkRunes
	// is the same signal when ChunkSize is small and the validator's
	// tiny-Chunk rule bites first.
	minAverageSectionFraction = 4
)

// SelectTiers returns the tier chain for a profiled Document: the heading
// tier then the recursive one when there are enough headed sections with
// body text, recursive alone otherwise. Long sections are no reason to skip
// the heading tier: it splits them inside their breadcrumb. cfg must have
// had its defaults applied; Split passes the config ensureDefaults returned.
func SelectTiers(p DocProfile, cfg SplitterConfig) []StrategyTier {
	lower := max(tinyChunkRunes, cfg.ChunkSize/minAverageSectionFraction)
	if p.HeadedSections < minHeadingsForHeadingTier || p.AverageHeadedSectionRunes < lower {
		return []StrategyTier{TierRecursive}
	}
	return []StrategyTier{TierHeading, TierRecursive}
}

// ProfileDocument scans text once and returns its DocProfile. It counts the
// sections headingSections returns, so the profile measures what the heading
// tier would emit.
func ProfileDocument(text string) DocProfile {
	p := DocProfile{Runes: utf8.RuneCountInString(text)}
	hs := headings(text)
	for _, h := range hs {
		p.HeadingsByLevel[h.level-1]++
	}
	total := 0
	for _, sec := range sectionsFor(text, hs) {
		if !sec.headed {
			continue
		}
		p.HeadedSections++
		total += utf8.RuneCountInString(text[sec.start:sec.end])
	}
	if p.HeadedSections > 0 {
		p.AverageHeadedSectionRunes = total / p.HeadedSections
	}
	return p
}
