package types

import (
	"fmt"
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
)

// RetrievalTopKDefault is how many Chunks a request that leaves top_k out
// gets.
const RetrievalTopKDefault = 10

// RetrievalTopKMax is the largest top_k a request may ask for.
const RetrievalTopKMax = 100

// RetrievalQueryMaxRunes is the longest query a request may carry, in runes.
// It keeps an arbitrary string out of a paid embedding call.
const RetrievalQueryMaxRunes = 2000

// KnownRetrievalTechniques returns the Retrieval Techniques this build
// implements, in the order an error message names them.
func KnownRetrievalTechniques() []RetrievalTechnique {
	return []RetrievalTechnique{RetrievalTechniqueDense, RetrievalTechniqueBM25}
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

// RetrievalConfig is a named combination of Retrieval Techniques switched on
// together (GLOSSARY.md).
type RetrievalConfig struct {
	// Techniques is the set of Retrieval Techniques to run, without
	// duplicates.
	Techniques []RetrievalTechnique `json:"techniques"`
}

// DefaultRetrievalConfig returns the config a request that leaves it out
// gets: dense alone.
func DefaultRetrievalConfig() *RetrievalConfig {
	return &RetrievalConfig{Techniques: []RetrievalTechnique{RetrievalTechniqueDense}}
}

// Validate rejects an empty list, a technique this build does not implement,
// a technique listed twice and more than one technique. The message names the
// offending technique and the ones a caller can use, except for the
// more-than-one rule, which names no technique because every listed one is
// valid on its own.
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
	if len(c.Techniques) > 1 {
		// #34's RRF fusion runs several techniques in one request; until it
		// lands, running two would need a way to combine their scores here.
		return fmt.Errorf("only one retrieval technique per request is supported until fusion lands")
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
// retrieval_config becomes dense alone. The error messages are fit for the
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
	return r.RetrievalConfig.Validate()
}

// ChunkMatch is one Chunk a Retrieval Technique found, with the score that
// technique gave it. Higher is more relevant.
type ChunkMatch struct {
	Chunk *Chunk
	Score float64
}

// RetrievedChunk is one Chunk in the answer to POST /retrieve, with the
// final score that orders the results and the score each technique gave it.
type RetrievedChunk struct {
	ChunkID       uuid.UUID                      `json:"chunk_id"`
	DocumentID    uuid.UUID                      `json:"document_id"`
	Seq           int                            `json:"seq"`
	Content       string                         `json:"content"`
	ContextHeader string                         `json:"context_header"`
	Score         float64                        `json:"score"`
	Scores        map[RetrievalTechnique]float64 `json:"scores"`
}

// The steps a retrieval run records in its trace.
const (
	// TraceStepEmbedQuery embeds the query text with the embedding role.
	TraceStepEmbedQuery = "embed_query"
	// TraceStepSearch searches the Chunks of the readable Documents.
	TraceStepSearch = "search"
)

// TraceStep is one step of a retrieval run, timed so a caller can see where
// the latency went. The optional fields depend on the step.
type TraceStep struct {
	// Step names what ran, e.g. TraceStepEmbedQuery.
	Step string `json:"step"`
	// Technique is the Retrieval Technique the step belongs to.
	Technique RetrievalTechnique `json:"technique,omitempty"`
	// DurationMS is how long the step took, in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// Model is the embedding model, on the step that called one.
	Model string `json:"model,omitempty"`
	// Candidates is how many Chunks the search returned, on the search step.
	// It is a pointer so the search step reports 0 while the embed step,
	// which has no candidate count, leaves the field out.
	Candidates *int `json:"candidates,omitempty"`
}

// RetrievalResult is the payload of a successful POST /retrieve: the ranked
// Chunks and the trace of the steps that produced them.
type RetrievalResult struct {
	Results []RetrievedChunk `json:"results"`
	Trace   []TraceStep      `json:"trace"`
}
