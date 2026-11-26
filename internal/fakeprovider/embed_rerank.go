package fakeprovider

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strings"
)

// SetEmbeddingDims sets the length of the vectors [Embeddings] returns for
// requests that do not ask for `dimensions` themselves.
func (p *Provider) SetEmbeddingDims(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dims = n
}

// EmbedAs pins the vector [Embeddings] returns for text, so a test can
// place texts near or far from each other. Other texts keep their hashed
// vectors.
func (p *Provider) EmbedAs(text string, vec []float32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.vectors[text] = slices.Clone(vec)
}

// ScoreAs pins the relevance [Rerank] gives document for query.
func (p *Provider) ScoreAs(query, document string, score float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scores[[2]string{query, document}] = score
}

// texts is an `input` or `documents` value: a string, or an array of
// strings or of objects with text.
type texts []string

func (t *texts) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*t = texts{s}
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(b, &items); err != nil {
		return err
	}
	out := make(texts, len(items))
	for i, item := range items {
		if err := json.Unmarshal(item, &out[i]); err == nil {
			continue
		}
		var doc struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item, &doc); err != nil {
			return err
		}
		out[i] = doc.Text
	}
	*t = out
	return nil
}

func (p *Provider) embeddings(_ http.ResponseWriter, body []byte) (int, any) {
	var req struct {
		Model      string `json:"model"`
		Input      texts  `json:"input"`
		Dimensions int    `json:"dimensions"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return badRequest(err)
	}
	type embedding struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	data := make([]embedding, len(req.Input))
	tokens := 0
	p.mu.Lock()
	dims := cmp.Or(req.Dimensions, p.dims)
	for i, text := range req.Input {
		vec, ok := p.vectors[text]
		if !ok {
			vec = hashVector(text, dims)
		}
		data[i] = embedding{Object: "embedding", Index: i, Embedding: vec}
		tokens += countTokens(text)
	}
	p.mu.Unlock()
	return http.StatusOK, map[string]any{
		"object": "list",
		"model":  req.Model,
		"data":   data,
		"usage":  map[string]int{"prompt_tokens": tokens, "total_tokens": tokens},
	}
}

// hashVector is a unit vector of length dims derived from text alone.
func hashVector(text string, dims int) []float32 {
	vec := make([]float32, dims)
	var norm float64
	for i := range vec {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(i))
		sum := sha256.Sum256(append(b[:], text...))
		x := float64(binary.BigEndian.Uint32(sum[:4]))/math.MaxUint32*2 - 1
		vec[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

func (p *Provider) rerank(_ http.ResponseWriter, body []byte) (int, any) {
	var req struct {
		Model           string `json:"model"`
		Query           string `json:"query"`
		Documents       texts  `json:"documents"`
		TopN            int    `json:"top_n"`
		ReturnDocuments bool   `json:"return_documents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return badRequest(err)
	}
	type document struct {
		Text string `json:"text"`
	}
	type result struct {
		Index          int       `json:"index"`
		RelevanceScore float64   `json:"relevance_score"`
		Document       *document `json:"document,omitempty"`
	}
	results := make([]result, len(req.Documents))
	tokens := countTokens(req.Query)
	p.mu.Lock()
	for i, doc := range req.Documents {
		score, ok := p.scores[[2]string{req.Query, doc}]
		if !ok {
			score = overlap(req.Query, doc)
		}
		results[i] = result{Index: i, RelevanceScore: score}
		if req.ReturnDocuments {
			results[i].Document = &document{Text: doc}
		}
		tokens += countTokens(doc)
	}
	p.mu.Unlock()
	slices.SortStableFunc(results, func(a, b result) int { return cmp.Compare(b.RelevanceScore, a.RelevanceScore) })
	if req.TopN > 0 && req.TopN < len(results) {
		results = results[:req.TopN]
	}
	return http.StatusOK, map[string]any{
		"model":   req.Model,
		"results": results,
		"usage":   map[string]int{"total_tokens": tokens},
	}
}

// overlap is the share of the query's distinct words, case-folded, that
// appear in doc: 1 when doc has them all, 0 when it has none.
func overlap(query, doc string) float64 {
	words := func(s string) []string {
		ws := strings.Fields(strings.ToLower(s))
		slices.Sort(ws)
		return slices.Compact(ws)
	}
	q, d := words(query), words(doc)
	if len(q) == 0 {
		return 0
	}
	hits := 0
	for _, w := range q {
		if _, ok := slices.BinarySearch(d, w); ok {
			hits++
		}
	}
	return float64(hits) / float64(len(q))
}
