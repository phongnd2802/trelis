package config

import (
	"fmt"

	"github.com/phongnd2802/trelis/internal/chunker"
)

// Chunking configures how ingest splits a Document's Markdown into Chunks,
// and optionally packs those Chunks into Parent Chunks. A Chunk is what
// retrieval embeds and searches; a Parent Chunk is wider context stored
// around consecutive Chunks and never embedded or searched itself
// (GLOSSARY.md).
type Chunking struct {
	// Strategy picks the tier chain: auto tries the structured tiers before
	// the recursive one, recursive runs the recursive tier alone.
	Strategy string `mapstructure:"strategy"`
	// ChunkSize is the target size of an embedded Chunk in runes.
	ChunkSize int `mapstructure:"chunk_size"`
	// ChunkOverlap is the most runes a Chunk repeats from the one before it.
	ChunkOverlap int `mapstructure:"chunk_overlap"`
	// ParentChild stores Parent Chunks around consecutive Chunks. Only the
	// Chunks are embedded and searched.
	ParentChild bool `mapstructure:"parent_child"`
	// ParentChunkSize is the target size of a Parent Chunk in runes, used
	// only when ParentChild is on.
	ParentChunkSize int `mapstructure:"parent_chunk_size"`
}

func init() {
	defaults["chunking.strategy"] = chunker.StrategyAuto
	defaults["chunking.chunk_size"] = chunker.DefaultChunkSize
	defaults["chunking.chunk_overlap"] = chunker.DefaultChunkOverlap
	defaults["chunking.parent_child"] = false
	defaults["chunking.parent_chunk_size"] = chunker.DefaultParentChunkSize
}

// SplitterConfig builds the chunker configuration these settings describe.
// ParentChunkSize is zero when parent-child mode is off, which makes the
// chunker return one Parent Chunk per Chunk; the service then stores only the
// Chunks.
func (c Chunking) SplitterConfig() chunker.SplitterConfig {
	cfg := chunker.DefaultConfig()
	cfg.Strategy = c.Strategy
	cfg.ChunkSize = c.ChunkSize
	cfg.ChunkOverlap = c.ChunkOverlap
	if c.ParentChild {
		cfg.ParentChunkSize = c.ParentChunkSize
	}
	return cfg
}

// validate reports every way the chunking settings cannot run as written. A
// Parent Chunk size below twice the Chunk size is an error rather than the
// chunker's silent raise: a config that says one thing and runs another
// misleads its operator.
func (c Chunking) validate() []error {
	var errs []error
	if c.Strategy != chunker.StrategyAuto && c.Strategy != chunker.StrategyRecursive {
		errs = append(errs, fmt.Errorf("chunking.strategy %q is not a strategy this build ships (want %s or %s)",
			c.Strategy, chunker.StrategyAuto, chunker.StrategyRecursive))
	}
	if c.ChunkSize < 1 {
		errs = append(errs, fmt.Errorf("chunking.chunk_size must be at least 1, got %d", c.ChunkSize))
	}
	if c.ChunkOverlap < 0 {
		errs = append(errs, fmt.Errorf("chunking.chunk_overlap must not be negative, got %d", c.ChunkOverlap))
	} else if c.ChunkOverlap >= c.ChunkSize {
		errs = append(errs, fmt.Errorf("chunking.chunk_overlap must be less than chunking.chunk_size (%d), got %d",
			c.ChunkSize, c.ChunkOverlap))
	}
	if c.ParentChild && c.ParentChunkSize < 2*c.ChunkSize {
		errs = append(errs, fmt.Errorf("chunking.parent_chunk_size must be at least twice chunking.chunk_size (%d) in parent-child mode, got %d",
			2*c.ChunkSize, c.ParentChunkSize))
	}
	return errs
}
