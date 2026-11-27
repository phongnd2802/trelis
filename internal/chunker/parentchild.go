package chunker

// ParentChildResult is a parent-child split: Parents are context windows,
// never embedded, and cover source ranges over consecutive children; they may
// overlap each other by up to ChunkOverlap. Children are Split's Chunks, each
// inside one parent.
type ParentChildResult struct {
	Parents  []Chunk
	Children []ChildChunk
}

// ChildChunk is one Chunk of a parent, with Parent indexing
// ParentChildResult.Parents.
type ChildChunk struct {
	Chunk
	Parent int
}

// SplitParentChild splits text into parent Chunks and the child Chunks to
// embed. Children are exactly Split's output at the configured ChunkSize.
// With SplitterConfig.ParentChunkSize set, each parent packs consecutive
// children into a window of at most that many runes; with it unset, every
// child is its own parent.
func SplitParentChild(text string, cfg SplitterConfig) ParentChildResult {
	result, _ := SplitParentChildWithDiagnostics(text, cfg)
	return result
}

// SplitParentChildWithDiagnostics is SplitParentChild, also reporting the
// child split's diagnostics: parents only group the Chunks that split
// returns. Adjacent children may overlap by up to ChunkOverlap, so adjacent
// parents may repeat at most that many runes; callers returning a parent per
// answer should deduplicate repeated parents.
func SplitParentChildWithDiagnostics(text string, cfg SplitterConfig) (ParentChildResult, *Diagnostics) {
	children, diag := SplitWithDiagnostics(text, cfg)
	if len(children) == 0 {
		return ParentChildResult{}, diag
	}
	cfg = NormalizeSplitterConfig(cfg)
	if cfg.ParentChunkSize == 0 {
		return oneChildPerParent(children), diag
	}
	parents, wrapped := packParents(text, children, cfg.ParentChunkSize)
	return ParentChildResult{Parents: parents, Children: wrapped}, diag
}

// packParents groups consecutive children into parent windows of at most
// maxRunes over the source range [first.Start, last.End). Packing is plain
// greedy; a parent can straddle a section boundary when both fit. A child
// larger than maxRunes on its own becomes a parent by itself.
func packParents(text string, children []Chunk, maxRunes int) ([]Chunk, []ChildChunk) {
	runes := []rune(text)
	parents := make([]Chunk, 0, len(children))
	wrapped := make([]ChildChunk, 0, len(children))
	for first := 0; first < len(children); {
		last := first
		for last+1 < len(children) && children[last+1].End-children[first].Start <= maxRunes {
			last++
		}
		pi := len(parents)
		parents = append(parents, Chunk{
			Content:       string(runes[children[first].Start:children[last].End]),
			ContextHeader: children[first].ContextHeader,
			Seq:           pi,
			Start:         children[first].Start,
			End:           children[last].End,
		})
		for i := first; i <= last; i++ {
			wrapped = append(wrapped, ChildChunk{Chunk: children[i], Parent: pi})
		}
		first = last + 1
	}
	return parents, wrapped
}

// oneChildPerParent is the parent-child result when the mode is off: every
// Chunk is a parent and its own child, numbered as the parent is.
func oneChildPerParent(children []Chunk) ParentChildResult {
	parents := make([]Chunk, len(children))
	wrapped := make([]ChildChunk, len(children))
	for i, c := range children {
		parents[i] = c
		wrapped[i] = ChildChunk{Chunk: c, Parent: i}
	}
	return ParentChildResult{Parents: parents, Children: wrapped}
}
