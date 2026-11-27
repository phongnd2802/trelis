package chunker

import "strings"

// StrategyTier names one chunking implementation in a tier chain.
type StrategyTier string

// TierRecursive is the recursive tier. It ends every chain. The heading and
// heuristic tiers may run before it.
const TierRecursive StrategyTier = "recursive"

// TierHeading is the heading tier. It splits a Markdown Document at its ATX
// headings and puts the heading context its Content lacks in ContextHeader.
const TierHeading StrategyTier = "heading"

// TierHeuristic is the heuristic tier. It finds section boundaries in plain
// lines, Vietnamese legal structure first, and puts that path in
// ContextHeader the same way the heading tier does.
const TierHeuristic StrategyTier = "heuristic"

// tierSplitters maps each tier to its implementation. Each receives a
// config with defaults applied.
var tierSplitters = map[StrategyTier]func(text string, cfg SplitterConfig) []Chunk{
	TierHeading:   splitHeading,
	TierHeuristic: splitHeuristic,
	TierRecursive: splitRecursive,
}

// TierRejection records a tier whose output ValidateChunks rejected, and
// why.
type TierRejection struct {
	Tier   StrategyTier `json:"tier"`
	Reason string       `json:"reason"`
}

// Diagnostics records how Split chose its Chunks: the chain it tried, the
// tiers rejected along the way, the tier whose output it returned, and the
// Document profile that chose the chain. Field names are stable, for a
// debug or preview view.
type Diagnostics struct {
	SelectedTier StrategyTier    `json:"selected_tier"`
	TierChain    []StrategyTier  `json:"tier_chain"`
	Rejected     []TierRejection `json:"rejected"`
	// Profile is nil unless StrategyAuto consulted the profiler.
	Profile *DocProfile `json:"profile,omitempty"`
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
	chain, profile := resolveChain(text, cfg)
	chunks, diag := runChain(text, cfg, chain)
	diag.Profile = profile
	return chunks, diag
}

// resolveChain returns the tiers to try, in order, ending with the
// recursive tier, and the profile that chose them (nil unless StrategyAuto).
// An empty Strategy means recursive, so configs stored before strategies
// existed keep their behaviour. StrategyAuto profiles the Document and picks
// the first structured tier whose sections are dense enough: heading before
// heuristic, since real Markdown headings are the stronger signal.
//
// An unknown strategy falls back to recursive, and Diagnostics.TierChain is
// where a library caller sees that: config validation rejects it before
// ingest, so the chunker logs nothing.
//
// A structured tier scans for its boundaries again rather than taking the
// profile's list: tierSplitters is text+cfg for every tier, and threading one
// scan through it would couple the chain to the profiler for a cheap line
// scan.
func resolveChain(text string, cfg SplitterConfig) ([]StrategyTier, *DocProfile) {
	switch cfg.Strategy {
	case StrategyRecursive, "":
	case StrategyAuto:
		p := ProfileDocument(text)
		return SelectTiers(p, cfg), &p
	}
	return []StrategyTier{TierRecursive}, nil
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
	}
	return out, diag
}
