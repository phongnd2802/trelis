package chunker

import (
	"log/slog"
	"strings"
)

// StrategyTier names one chunking implementation in a tier chain.
type StrategyTier string

// TierRecursive is the recursive tier. It ends every chain. The heading
// tier (#28) and the heuristic tier (#29) are added before it.
const TierRecursive StrategyTier = "recursive"

// tierSplitters maps each tier to its implementation. Each receives a
// config with defaults applied.
var tierSplitters = map[StrategyTier]func(text string, cfg SplitterConfig) []Chunk{
	TierRecursive: splitRecursive,
}

// TierRejection records a tier whose output ValidateChunks rejected, and
// why.
type TierRejection struct {
	Tier   StrategyTier `json:"tier"`
	Reason string       `json:"reason"`
}

// Diagnostics records how Split chose its Chunks: the chain it tried, the
// tiers rejected along the way, and the tier whose output it returned.
// Field names are stable, for a debug or preview view.
type Diagnostics struct {
	SelectedTier StrategyTier    `json:"selected_tier"`
	TierChain    []StrategyTier  `json:"tier_chain"`
	Rejected     []TierRejection `json:"rejected"`
}

// Split splits text into Chunks with the tier chain cfg.Strategy selects.
// Blank text has no Chunks.
func Split(text string, cfg SplitterConfig) []Chunk {
	chunks, _ := SplitWithDiagnostics(text, cfg)
	return chunks
}

// SplitWithDiagnostics is Split, also reporting which tiers ran and why
// any were rejected.
func SplitWithDiagnostics(text string, cfg SplitterConfig) ([]Chunk, *Diagnostics) {
	if strings.TrimSpace(text) == "" {
		return nil, &Diagnostics{SelectedTier: TierRecursive}
	}
	cfg = ensureDefaults(cfg, text)
	return runChain(text, cfg, resolveChain(cfg))
}

// resolveChain returns the tiers to try, in order, ending with the
// recursive tier. An empty Strategy means recursive, so configs stored
// before strategies existed keep their behaviour. StrategyAuto will consult
// the document profiler (#28); until then it, too, runs the recursive tier
// alone.
func resolveChain(cfg SplitterConfig) []StrategyTier {
	switch cfg.Strategy {
	case StrategyRecursive, "":
	case StrategyAuto:
	default:
		slog.Warn("chunker: unknown strategy, using recursive", "strategy", cfg.Strategy)
	}
	return []StrategyTier{TierRecursive}
}

// runChain runs each tier in chain until ValidateChunks accepts one's
// output. The last tier's output is returned even when rejected, so
// callers always get Chunks.
func runChain(text string, cfg SplitterConfig, chain []StrategyTier) ([]Chunk, *Diagnostics) {
	diag := &Diagnostics{TierChain: chain}
	var out []Chunk
	for _, tier := range chain {
		out = tierSplitters[tier](text, cfg)
		diag.SelectedTier = tier
		v := ValidateChunks(text, out, cfg)
		if v.OK {
			return out, diag
		}
		diag.Rejected = append(diag.Rejected, TierRejection{Tier: tier, Reason: v.Reason})
		slog.Debug("chunker: tier rejected", "tier", tier, "reason", v.Reason)
	}
	return out, diag
}
