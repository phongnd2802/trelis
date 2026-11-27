package chunker

import "strings"

// Strategy values for SplitterConfig.Strategy.
const (
	// StrategyAuto profiles the Document and lets it decide which tiers to
	// try: dense Markdown headings get the heading tier then recursive,
	// otherwise enough heuristic sections get the heuristic tier then
	// recursive, and anything else the recursive tier alone.
	StrategyAuto = "auto"
	// StrategyRecursive runs the recursive tier alone.
	StrategyRecursive = "recursive"
)

// Default sizes, in runes. 512 runes is a good general-purpose baseline;
// 80 runes of overlap (about 15%) lets an answer that straddles a boundary
// still be found, at little storage cost. Use 0 overlap for atomic content
// such as FAQ entries.
const (
	DefaultChunkSize    = 512
	DefaultChunkOverlap = 80
)

// SplitterConfig configures how text is split into Chunks.
type SplitterConfig struct {
	// ChunkSize is the target Chunk size in runes.
	ChunkSize int
	// ChunkOverlap is the most runes a Chunk repeats from the end of the
	// one before it. Overlap always starts at a paragraph, line or sentence
	// boundary; with no such boundary in range there is none. 0 with
	// ChunkSize set means no overlap; with both unset the default applies.
	ChunkOverlap int
	// Separators are tried in order: a piece still over ChunkSize after
	// splitting at one is split at the next.
	Separators []string
	// Strategy picks the tier chain: StrategyRecursive (also the empty
	// value) or StrategyAuto.
	Strategy string
	// TokenLimit caps Chunks in approximate tokens, usually the embedding
	// model's input limit. When set, ChunkSize is lowered to the rune
	// budget that fits it, and no Chunk, protected spans included, is
	// larger than that budget.
	TokenLimit int
	// Languages hints the language for token estimates. Empty means
	// detect it from the text.
	Languages []string
}

// DefaultConfig returns the default SplitterConfig.
func DefaultConfig() SplitterConfig {
	return SplitterConfig{
		ChunkSize:    DefaultChunkSize,
		ChunkOverlap: DefaultChunkOverlap,
		Separators:   defaultSeparators(),
	}
}

// defaultSeparators go from paragraphs down to sentence ends, in the
// scripts of both Corpora (Latin and CJK punctuation).
func defaultSeparators() []string {
	return []string{"\n\n", "\n", "。", ". ", "? ", "! ", "; "}
}

// NormalizeSplitterConfig fills unset size, overlap and separator fields
// with the defaults and caps the overlap, the same way Split does, so
// callers that store or show a config see what will run. Split further
// lowers ChunkSize to fit TokenLimit, which can depend on the text.
func NormalizeSplitterConfig(cfg SplitterConfig) SplitterConfig {
	if cfg.ChunkSize <= 0 {
		// A config that sets neither size nor overlap wants both defaults;
		// zero overlap next to an explicit size means no overlap.
		cfg.ChunkSize = DefaultChunkSize
		if cfg.ChunkOverlap == 0 {
			cfg.ChunkOverlap = DefaultChunkOverlap
		}
	}
	if cfg.ChunkOverlap < 0 {
		cfg.ChunkOverlap = DefaultChunkOverlap
	}
	if len(cfg.Separators) == 0 {
		cfg.Separators = defaultSeparators()
	}
	return capOverlap(cfg)
}

// capOverlap keeps overlap at most half a Chunk: more makes most Chunks
// near-copies of the previous one.
func capOverlap(cfg SplitterConfig) SplitterConfig {
	cfg.ChunkOverlap = min(cfg.ChunkOverlap, cfg.ChunkSize/2)
	return cfg
}

// ensureDefaults is NormalizeSplitterConfig plus the token limit, which
// needs the text when no language is given.
func ensureDefaults(cfg SplitterConfig, text string) SplitterConfig {
	cfg = NormalizeSplitterConfig(cfg)
	if cfg.TokenLimit > 0 {
		if budget := CharsForTokenLimit(cfg.TokenLimit, tokenLanguage(cfg, text)); budget > 0 && budget < cfg.ChunkSize {
			cfg.ChunkSize = budget
		}
	}
	return capOverlap(cfg)
}

// tokenLanguage is the language token estimates use: the first hint in
// cfg.Languages, or else the one detected in text.
func tokenLanguage(cfg SplitterConfig, text string) string {
	if len(cfg.Languages) > 0 {
		return cfg.Languages[0]
	}
	return DetectLanguage(text)
}

// NormalizeLineEndings turns CRLF and CR into LF, so the same Document
// uploaded from different systems splits the same way.
func NormalizeLineEndings(text string) string {
	if !strings.Contains(text, "\r") {
		return text
	}
	return strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
}
