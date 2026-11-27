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
	// RetrievalTechniqueRerank reorders the Chunks another technique found
	// with the rerank model's relevance score. It is a second stage, not a
	// candidate source, so a request must name at least one other technique.
	RetrievalTechniqueRerank RetrievalTechnique = "rerank"
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
	return []RetrievalTechnique{RetrievalTechniqueDense, RetrievalTechniqueBM25, RetrievalTechniqueRerank}
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

// knownTechniqueList names the techniques this build implements, as an error
// message lists them.
func knownTechniqueList() string {
	names := make([]string, 0, len(KnownRetrievalTechniques()))
	for _, t := range KnownRetrievalTechniques() {
		names = append(names, fmt.Sprintf("%q", string(t)))
	}
	return strings.Join(names, ", ")
}

// knownCandidateTechniqueList names the techniques that find candidates, all
// but rerank, as the rerank-alone error lists them.
func knownCandidateTechniqueList() string {
	names := make([]string, 0, len(KnownRetrievalTechniques()))
	for _, t := range KnownRetrievalTechniques() {
		if t == RetrievalTechniqueRerank {
			continue
		}
		names = append(names, fmt.Sprintf("%q", string(t)))
	}
	return strings.Join(names, ", ")
}

// RetrievalConfig is a named combination of Retrieval Techniques switched on
// together (GLOSSARY.md).
type RetrievalConfig struct {
	// Techniques is the set of Retrieval Techniques to run, without
	// duplicates. More than one candidate technique makes the request a
	// fusion: each runs over the readable Documents and their lists are
	// combined with Reciprocal Rank Fusion. Rerank reorders whatever the
	// candidate techniques found.
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
// requested technique except rerank, which only reorders them. Validation
// rejects a config that names rerank alone, so a valid config's result is
// never empty.
func (c *RetrievalConfig) CandidateTechniques() []RetrievalTechnique {
	out := make([]RetrievalTechnique, 0, len(c.Techniques))
	for _, technique := range c.Techniques {
		if technique != RetrievalTechniqueRerank {
			out = append(out, technique)
		}
	}
	return out
}

// Reranks reports whether the config names the rerank technique.
func (c *RetrievalConfig) Reranks() bool {
	return slices.Contains(c.Techniques, RetrievalTechniqueRerank)
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
// a technique listed twice, rerank on its own, an rrf_k outside
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
	if c.Reranks() && len(c.CandidateTechniques()) == 0 {
		return fmt.Errorf("retrieval technique %q reorders the Chunks another technique finds, so techniques must name at least one candidate technique as well (candidate techniques: %s)",
			RetrievalTechniqueRerank, knownCandidateTechniqueList())
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
	Score float64
}

// RetrievedChunk is one Chunk in the answer to POST /retrieve, with the
// final score that orders the results and, for every technique whose list
// carried it, that technique's own score and 1-based rank.
type RetrievedChunk struct {
	ChunkID       uuid.UUID                      `json:"chunk_id"`
	DocumentID    uuid.UUID                      `json:"document_id"`
	Seq           int                            `json:"seq"`
	Content       string                         `json:"content"`
	ContextHeader string                         `json:"context_header"`
	Score         float64                        `json:"score"`
	Scores        map[RetrievalTechnique]float64 `json:"scores"`
	Ranks         map[RetrievalTechnique]int     `json:"ranks"`
}

// The steps a retrieval run records in its trace.
const (
	// TraceStepEmbedQuery embeds the query text with the embedding role.
	TraceStepEmbedQuery = "embed_query"
	// TraceStepSearch searches the Chunks of the readable Documents.
	TraceStepSearch = "search"
	// TraceStepFuse combines the techniques' ranked lists with Reciprocal
	// Rank Fusion.
	TraceStepFuse = "fuse"
	// TraceStepRerank reorders the candidate pool with the rerank model.
	TraceStepRerank = "rerank"
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
	// Model is the embedding or rerank model, on the step that called one.
	Model string `json:"model,omitempty"`
	// Candidates is how many Chunks the step returned: the search step's own
	// matches, the distinct Chunks a fuse step combined, or the pool size sent
	// to the reranker. It is a pointer so a step with no candidate count
	// leaves the field out.
	Candidates *int `json:"candidates,omitempty"`
}

// RetrievalResult is the payload of a successful POST /retrieve: the ranked
// Chunks and the trace of the steps that produced them.
type RetrievalResult struct {
	Results []RetrievedChunk `json:"results"`
	Trace   []TraceStep      `json:"trace"`
}
