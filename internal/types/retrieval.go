package types

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// RetrievalTechnique is one way of finding evidence that can be switched on
// or off on its own (GLOSSARY.md).
type RetrievalTechnique string

// The Retrieval Techniques this build implements.
const (
	// RetrievalTechniqueDense ranks Chunks by the cosine similarity between
	// their embedding and the query's.
	RetrievalTechniqueDense RetrievalTechnique = "dense"
	// RetrievalTechniqueBM25 ranks Chunks by pg_search's BM25 score over their
	// text at Vietnamese syllable level, plus a diacritics-free copy so a
	// query typed without diacritics still matches.
	RetrievalTechniqueBM25 RetrievalTechnique = "bm25"
	// RetrievalTechniqueQueryRewrite rewrites the latest question into one
	// standalone query using the Conversation History, so a follow-up such as
	// "and what about 2024?" searches for what it refers to. It is a first
	// stage, not a candidate source, so a request must name at least one other
	// technique.
	RetrievalTechniqueQueryRewrite RetrievalTechnique = "query_rewrite"
	// RetrievalTechniqueRerank reorders the Chunks another technique found
	// with the rerank model's relevance score. It is a second stage, not a
	// candidate source, so a request must name at least one other technique.
	RetrievalTechniqueRerank RetrievalTechnique = "rerank"
	// RetrievalTechniqueParentChild replaces the Chunks another technique
	// found with their Parent Chunks, collapsing several Chunks of one Parent
	// Chunk into one result. It is a final stage, not a candidate source, so a
	// request must name at least one other technique.
	RetrievalTechniqueParentChild RetrievalTechnique = "parent_child"
)

// RetrievalTechniqueRRF labels the step that fuses several techniques' lists
// in a trace. It is not a technique a caller can switch on, so Known reports
// false for it and KnownRetrievalTechniques leaves it out.
const RetrievalTechniqueRRF RetrievalTechnique = "rrf"

// RetrievalTopKDefault is how many Chunks a request that leaves top_k out
// gets.
const RetrievalTopKDefault = 10

// RetrievalTopKMax is the largest top_k a request may ask for.
const RetrievalTopKMax = 100

// RetrievalRRFKDefault is the Reciprocal Rank Fusion constant k a request that
// leaves rrf_k out gets.
const RetrievalRRFKDefault = 60

// RetrievalRRFKMax is the largest rrf_k a request may ask for.
const RetrievalRRFKMax = 1000

// RetrievalFusionCandidates is how many candidates each technique fetches
// before fusion when a request runs more than one technique. A Chunk that
// every list places low can still reach the fused top_k that way.
const RetrievalFusionCandidates = 50

// RetrievalRerankCandidatesDefault is how many candidates the first stage
// hands to the reranker when a request leaves rerank_candidates out.
const RetrievalRerankCandidatesDefault = 50

// RetrievalRerankCandidatesMax is the largest candidate pool a request may
// ask the reranker to score.
const RetrievalRerankCandidatesMax = 100

// RetrievalQueryMaxRunes is the longest query a request may carry, in runes.
// It keeps an arbitrary string out of a paid embedding call.
const RetrievalQueryMaxRunes = 2000

// KnownRetrievalTechniques returns the Retrieval Techniques this build
// implements, in the order an error message names them.
func KnownRetrievalTechniques() []RetrievalTechnique {
	return []RetrievalTechnique{
		RetrievalTechniqueDense,
		RetrievalTechniqueBM25,
		RetrievalTechniqueQueryRewrite,
		RetrievalTechniqueRerank,
		RetrievalTechniqueParentChild,
	}
}

// Known reports whether this build implements the technique. A technique a
// later build adds is unknown here until it is registered.
func (t RetrievalTechnique) Known() bool {
	for _, known := range KnownRetrievalTechniques() {
		if t == known {
			return true
		}
	}
	return false
}

// IsStage reports whether the technique only transforms another technique's
// candidates, rather than finding candidates of its own.
func (t RetrievalTechnique) IsStage() bool {
	return t == RetrievalTechniqueQueryRewrite || t == RetrievalTechniqueRerank || t == RetrievalTechniqueParentChild
}

// quotedTechniqueList names techniques as an error message lists them.
func quotedTechniqueList(techniques []RetrievalTechnique) string {
	names := make([]string, 0, len(techniques))
	for _, t := range techniques {
		names = append(names, fmt.Sprintf("%q", string(t)))
	}
	return strings.Join(names, ", ")
}

// knownTechniqueList names the techniques this build implements, as an error
// message lists them.
func knownTechniqueList() string {
	return quotedTechniqueList(KnownRetrievalTechniques())
}

// knownCandidateTechniqueList names the techniques that find candidates, all
// but the stage techniques, as an error that rejects a stage-only config
// lists them.
func knownCandidateTechniqueList() string {
	candidates := make([]RetrievalTechnique, 0, len(KnownRetrievalTechniques()))
	for _, t := range KnownRetrievalTechniques() {
		if !t.IsStage() {
			candidates = append(candidates, t)
		}
	}
	return quotedTechniqueList(candidates)
}

// stageOnlyMessage says why a config whose techniques are all stages cannot
// run, naming the techniques it named.
func stageOnlyMessage(techniques []RetrievalTechnique) string {
	if len(techniques) == 1 {
		switch techniques[0] {
		case RetrievalTechniqueQueryRewrite:
			return fmt.Sprintf("retrieval technique %q rewrites the query another technique searches with",
				RetrievalTechniqueQueryRewrite)
		case RetrievalTechniqueRerank:
			return fmt.Sprintf("retrieval technique %q reorders the Chunks another technique finds",
				RetrievalTechniqueRerank)
		case RetrievalTechniqueParentChild:
			return fmt.Sprintf("retrieval technique %q expands the Chunks another technique finds into their Parent Chunks",
				RetrievalTechniqueParentChild)
		}
	}
	return fmt.Sprintf("retrieval techniques %s only rewrite the query, reorder or expand the Chunks another technique finds",
		quotedTechniqueList(techniques))
}

// RetrievalConfig is a named combination of Retrieval Techniques switched on
// together (GLOSSARY.md).
type RetrievalConfig struct {
	// Techniques is the set of Retrieval Techniques to run, without
	// duplicates. More than one candidate technique makes the request a
	// fusion: each runs over the readable Documents and their lists are
	// combined with Reciprocal Rank Fusion. query_rewrite rewrites the
	// question first when the request carries Conversation History; rerank
	// reorders whatever the candidate techniques found; parent_child then
	// replaces the ordered Chunks with their Parent Chunks.
	Techniques []RetrievalTechnique `json:"techniques"`
	// RRFK is the Reciprocal Rank Fusion constant k; nil means the default.
	// It only affects a request that runs more than one technique, so it is
	// accepted and ignored for a single one.
	RRFK *int `json:"rrf_k,omitempty"`
	// RerankCandidates is how many Chunks the first stage hands to the
	// reranker; nil means the default. It is accepted and ignored when the
	// list does not name the rerank technique.
	RerankCandidates *int `json:"rerank_candidates,omitempty"`
}

// CandidateTechniques returns the techniques that find candidates: every
// requested technique except the stage techniques, query_rewrite, rerank and
// parent_child. Validation rejects a config that names only stage techniques,
// so a valid config's result is never empty.
func (c *RetrievalConfig) CandidateTechniques() []RetrievalTechnique {
	out := make([]RetrievalTechnique, 0, len(c.Techniques))
	for _, technique := range c.Techniques {
		if !technique.IsStage() {
			out = append(out, technique)
		}
	}
	return out
}

// RewritesQuery reports whether the config names the query_rewrite technique.
func (c *RetrievalConfig) RewritesQuery() bool {
	return slices.Contains(c.Techniques, RetrievalTechniqueQueryRewrite)
}

// Reranks reports whether the config names the rerank technique.
func (c *RetrievalConfig) Reranks() bool {
	return slices.Contains(c.Techniques, RetrievalTechniqueRerank)
}

// ExpandsParents reports whether the config names the parent_child technique.
func (c *RetrievalConfig) ExpandsParents() bool {
	return slices.Contains(c.Techniques, RetrievalTechniqueParentChild)
}

// DefaultRetrievalConfig returns the config a request that leaves it out
// gets: dense alone.
func DefaultRetrievalConfig() *RetrievalConfig {
	rrfK := RetrievalRRFKDefault
	candidates := RetrievalRerankCandidatesDefault
	return &RetrievalConfig{
		Techniques:       []RetrievalTechnique{RetrievalTechniqueDense},
		RRFK:             &rrfK,
		RerankCandidates: &candidates,
	}
}

// Validate rejects an empty list, a technique this build does not implement,
// a technique listed twice, a config whose only techniques are stage
// techniques (query_rewrite, rerank, parent_child), an rrf_k outside
// 1..RetrievalRRFKMax and a rerank_candidates outside
// 1..RetrievalRerankCandidatesMax. It fills an omitted rrf_k with
// RetrievalRRFKDefault and an omitted rerank_candidates with
// RetrievalRerankCandidatesDefault. The messages are fit for the client.
func (c *RetrievalConfig) Validate() error {
	if c == nil || len(c.Techniques) == 0 {
		return fmt.Errorf("retrieval_config.techniques must name at least one technique (known techniques: %s)",
			knownTechniqueList())
	}
	seen := make(map[RetrievalTechnique]struct{}, len(c.Techniques))
	for _, technique := range c.Techniques {
		if !technique.Known() {
			return fmt.Errorf("retrieval technique %q is not implemented (known techniques: %s)",
				technique, knownTechniqueList())
		}
		if _, duplicate := seen[technique]; duplicate {
			return fmt.Errorf("retrieval technique %q is listed more than once", technique)
		}
		seen[technique] = struct{}{}
	}
	if len(c.CandidateTechniques()) == 0 {
		return fmt.Errorf("%s, so techniques must name at least one candidate technique as well (candidate techniques: %s)",
			stageOnlyMessage(c.Techniques), knownCandidateTechniqueList())
	}
	if c.RRFK == nil {
		rrfK := RetrievalRRFKDefault
		c.RRFK = &rrfK
	}
	if *c.RRFK < 1 || *c.RRFK > RetrievalRRFKMax {
		return fmt.Errorf("retrieval_config.rrf_k must be between 1 and %d", RetrievalRRFKMax)
	}
	if c.RerankCandidates == nil {
		candidates := RetrievalRerankCandidatesDefault
		c.RerankCandidates = &candidates
	}
	if *c.RerankCandidates < 1 || *c.RerankCandidates > RetrievalRerankCandidatesMax {
		return fmt.Errorf("retrieval_config.rerank_candidates must be between 1 and %d",
			RetrievalRerankCandidatesMax)
	}
	return nil
}

// RetrievalRequest is the body of POST /retrieve.
type RetrievalRequest struct {
	// Query is the question to find Chunks for.
	Query string `json:"query"`
	// TopK is how many Chunks to return; nil means the default. An explicit
	// 0 is invalid, so a client that sends it learns it is out of range
	// instead of silently getting the default.
	TopK *int `json:"top_k"`
	// RetrievalConfig is the combination of techniques to run; nil means
	// dense alone.
	RetrievalConfig *RetrievalConfig `json:"retrieval_config,omitempty"`
	// History is the Conversation History a query rewrite resolves the
	// question against, oldest turn first. The answer service fills it; it is
	// never part of a request body, so POST /retrieve has none and a
	// query_rewrite request there runs no model call.
	History []ConversationTurn `json:"-"`
}

// Validate checks the request and fills its defaults: the query is trimmed,
// an omitted top_k becomes RetrievalTopKDefault, and an omitted
// retrieval_config becomes dense alone. A rerank_candidates smaller than
// top_k is rejected, since that pool could never fill top_k; it is ignored
// when the config does not name rerank. The error messages are fit for the
// client.
func (r *RetrievalRequest) Validate() error {
	r.Query = strings.TrimSpace(r.Query)
	if r.Query == "" {
		return fmt.Errorf("query must not be blank")
	}
	if n := utf8.RuneCountInString(r.Query); n > RetrievalQueryMaxRunes {
		return fmt.Errorf("query must be at most %d characters, got %d", RetrievalQueryMaxRunes, n)
	}
	if r.TopK == nil {
		topK := RetrievalTopKDefault
		r.TopK = &topK
	}
	if *r.TopK < 1 || *r.TopK > RetrievalTopKMax {
		return fmt.Errorf("top_k must be between 1 and %d", RetrievalTopKMax)
	}
	if r.RetrievalConfig == nil {
		r.RetrievalConfig = DefaultRetrievalConfig()
	}
	if err := r.RetrievalConfig.Validate(); err != nil {
		return err
	}
	if r.RetrievalConfig.Reranks() && *r.RetrievalConfig.RerankCandidates < *r.TopK {
		return fmt.Errorf("retrieval_config.rerank_candidates must be at least top_k (%d), got %d",
			*r.TopK, *r.RetrievalConfig.RerankCandidates)
	}
	return nil
}

// ChunkMatch is one Chunk a Retrieval Technique found, with the score that
// technique gave it. Higher is more relevant.
type ChunkMatch struct {
	Chunk *Chunk
	// ParentID is the Parent Chunk the Chunk belongs to, or nil when the
	// Document was ingested without parent-child mode.
	ParentID *uuid.UUID
	Score    float64
}

// RetrievedChunk is one result in the answer to POST /retrieve, with the
// final score that orders it and, for every technique whose list carried it,
// that technique's own score and 1-based rank. With parent-child it carries
// the best-ranked matched Chunk's ids and the Parent Chunk's text.
type RetrievedChunk struct {
	// ChunkID, DocumentID and Seq are the best-ranked matched Chunk's, so a
	// citation or GET /chunks/{id} still reaches a real Chunk.
	ChunkID    uuid.UUID `json:"chunk_id"`
	DocumentID uuid.UUID `json:"document_id"`
	Seq        int       `json:"seq"`
	// Content and ContextHeader are the Parent Chunk's when the request
	// expanded to parents, else the Chunk's own.
	Content       string                         `json:"content"`
	ContextHeader string                         `json:"context_header"`
	Score         float64                        `json:"score"`
	Scores        map[RetrievalTechnique]float64 `json:"scores"`
	Ranks         map[RetrievalTechnique]int     `json:"ranks"`
	// ParentChunkID is the Parent Chunk the content came from; it is absent
	// when the Chunk stands alone.
	ParentChunkID *uuid.UUID `json:"parent_chunk_id,omitempty"`
}

// The steps a retrieval run records in its trace.
const (
	// TraceStepRewriteQuery rewrites the latest question into a standalone
	// query with the chat role, using the Conversation History.
	TraceStepRewriteQuery = "rewrite_query"
	// TraceStepEmbedQuery embeds the query text with the embedding role.
	TraceStepEmbedQuery = "embed_query"
	// TraceStepSearch searches the Chunks of the readable Documents.
	TraceStepSearch = "search"
	// TraceStepFuse combines the techniques' ranked lists with Reciprocal
	// Rank Fusion.
	TraceStepFuse = "fuse"
	// TraceStepRerank reorders the candidate pool with the rerank model.
	TraceStepRerank = "rerank"
	// TraceStepExpandParents replaces matched Chunks with their Parent Chunks
	// and collapses Chunks of one Parent Chunk into one result.
	TraceStepExpandParents = "expand_parents"
)

// TraceStep is one step of a retrieval run, timed so a caller can see where
// the latency went. The optional fields depend on the step.
type TraceStep struct {
	// Step names what ran, e.g. TraceStepEmbedQuery.
	Step string `json:"step"`
	// Technique is the Retrieval Technique the step belongs to, or
	// RetrievalTechniqueRRF on the fuse step.
	Technique RetrievalTechnique `json:"technique,omitempty"`
	// DurationMS is how long the step took, in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// Model is the embedding, rerank or chat model, on the step that called
	// one.
	Model string `json:"model,omitempty"`
	// Candidate count fields below keep their pointer shape so a step that
	// has no count leaves the field out.
	//
	// Candidates is how many Chunks the step returned: the search step's own
	// matches, the distinct Chunks a fuse step combined, the pool size sent
	// to the reranker, or the results the expand_parents step produced before
	// the cut.
	Candidates *int `json:"candidates,omitempty"`
	// Expanded says which Chunks collapsed into which result, one entry per
	// returned result in result order, on the expand_parents step. It is a
	// pointer so an expand step with no results still answers expanded: []
	// while every other step leaves the field out.
	Expanded *[]ParentExpansion `json:"expanded,omitempty"`
	// RewrittenQuery is the query the techniques searched with, on the
	// rewrite_query step: the model's reply, or the raw question when the
	// reply was blank or too long.
	RewrittenQuery string `json:"rewritten_query,omitempty"`
	// Usage is the chat call's token accounting on the rewrite_query step.
	Usage *TokenUsage `json:"usage,omitempty"`
}

// ParentExpansion records how one parent_child result was built: its
// best-ranked matched Chunk, the Parent Chunk that supplied the content, and
// every matched Chunk that collapsed into it.
type ParentExpansion struct {
	// ChunkID is the result's chunk_id, the best-ranked matched Chunk.
	ChunkID uuid.UUID `json:"chunk_id"`
	// ParentChunkID is the Parent Chunk that supplied the content; nil when
	// the result is a Chunk with no Parent Chunk.
	ParentChunkID *uuid.UUID `json:"parent_chunk_id"`
	// MatchedChunkIDs is every matched Chunk that collapsed into the result,
	// in rank order; the first is ChunkID.
	MatchedChunkIDs []uuid.UUID `json:"matched_chunk_ids"`
}

// RetrievalResult is the payload of a successful POST /retrieve: the ranked
// Chunks and the trace of the steps that produced them.
type RetrievalResult struct {
	Results []RetrievedChunk `json:"results"`
	Trace   []TraceStep      `json:"trace"`
	// Query is the query retrieval actually ran with: the rewritten query
	// when query_rewrite ran, else the raw question. It is not part of the
	// response, so a caller that rewrites (the answer service) generates with
	// it while the wire shape stays what POST /retrieve always answered.
	Query string `json:"-"`
	// RewrittenQuery is the query a rewrite produced and retrieval searched
	// with, nil when no rewrite ran. It is the query actually used, so a
	// rewrite that fell back to the raw question still sets it. The
	// rewrite_query trace step records it on the wire.
	RewrittenQuery *string `json:"-"`
	// RewriteUsage is the chat call's token usage when a rewrite ran, and the
	// zero value when none did, so a caller can account for every model call.
	RewriteUsage TokenUsage `json:"-"`
}
