package chunker

import "strings"

// Strategy values for SplitterConfig.Strategy.
const (
	// StrategyAuto lets the document decide which tiers to try. Until the
	// profiler lands (#28) it runs the recursive tier alone.
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
	// Strategy picks the tier chain: StrategyAuto (also the empty value)
	// or StrategyRecursive.
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
// with the defaults, the same way Split does, so callers that store or show
// a config see what will run.
func NormalizeSplitterConfig(cfg SplitterConfig) SplitterConfig {
	sizeUnset := cfg.ChunkSize <= 0
	if sizeUnset {
		cfg.ChunkSize = DefaultChunkSize
	}
	if cfg.ChunkOverlap < 0 || (cfg.ChunkOverlap == 0 && sizeUnset) {
		cfg.ChunkOverlap = DefaultChunkOverlap
	}
	if len(cfg.Separators) == 0 {
		cfg.Separators = defaultSeparators()
	}
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
	// Overlap above half a Chunk makes most Chunks near-copies of the
	// previous one.
	cfg.ChunkOverlap = min(cfg.ChunkOverlap, cfg.ChunkSize/2)
	return cfg
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
