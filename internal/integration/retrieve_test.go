//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// retrieveQuery is the query every retrieve test pins its vector for.
const retrieveQuery = "who won the 2024 election?"

// retrievedChunk is the part of one result the tests read.
type retrievedChunk struct {
	ChunkID       string             `json:"chunk_id"`
	DocumentID    string             `json:"document_id"`
	Seq           int                `json:"seq"`
	Content       string             `json:"content"`
	ContextHeader string             `json:"context_header"`
	Score         float64            `json:"score"`
	Scores        map[string]float64 `json:"scores"`
	Ranks         map[string]int     `json:"ranks"`
}

// traceStep is one timed step of a retrieval run.
type traceStep struct {
	Step       string `json:"step"`
	Technique  string `json:"technique"`
	DurationMS int64  `json:"duration_ms"`
	Model      string `json:"model"`
	Candidates int    `json:"candidates"`
}

// retrievalAnswer is the envelope POST /retrieve answers with.
type retrievalAnswer struct {
	Success bool `json:"success"`
	Data    struct {
		Results []retrievedChunk `json:"results"`
		Trace   []traceStep      `json:"trace"`
	} `json:"data"`
}

// retrieve POSTs /retrieve asking for the named technique alone and returns
// the decoded answer, failing unless the request succeeded.
func (tr *trelis) retrieve(t *testing.T, key, query, technique string, topK int) retrievalAnswer {
	t.Helper()
	return tr.retrieveConfig(t, key, query, topK, map[string]any{"techniques": []string{technique}})
}

// retrieveConfig POSTs /retrieve with the given retrieval_config and returns
// the decoded answer, failing unless the request succeeded.
func (tr *trelis) retrieveConfig(t *testing.T, key, query string, topK int, config map[string]any) retrievalAnswer {
	t.Helper()
	resp := tr.post(t, "/retrieve", key, map[string]any{
		"query":            query,
		"top_k":            topK,
		"retrieval_config": config,
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /retrieve (%v) = %d %s, want 200", config["techniques"], resp.StatusCode, body)
	}
	var answer retrievalAnswer
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decode /retrieve (%v) answer: %v", config["techniques"], err)
	}
	if !answer.Success {
		t.Fatalf("POST /retrieve (%v) answered success = false", config["techniques"])
	}
	return answer
}

// chunkVectors reads a Document's stored embeddings straight from Postgres,
// in seq order, so a test can pin a query vector to one it knows the search
// compares against.
func (tr *trelis) chunkVectors(t *testing.T, documentID string) [][]float32 {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, deps.pg.host, deps.pg.port, tr.dbname))
	if err != nil {
		t.Fatalf("connect to database %s: %v", tr.dbname, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx,
		`SELECT embedding::text FROM chunks WHERE document_id = $1 AND embedding IS NOT NULL ORDER BY seq`, documentID)
	if err != nil {
		t.Fatalf("select embeddings of %s: %v", documentID, err)
	}
	defer rows.Close()
	var vectors [][]float32
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		vectors = append(vectors, parseVector(t, text))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return vectors
}

// parseVector reads pgvector's text form, [0.1,0.2].
func parseVector(t *testing.T, text string) []float32 {
	t.Helper()
	inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(text), "["), "]")
	if inner == "" {
		return nil
	}
	parts := strings.Split(inner, ",")
	vec := make([]float32, len(parts))
	for i, part := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 32)
		if err != nil {
			t.Fatalf("parse vector %q: %v", text, err)
		}
		vec[i] = float32(f)
	}
	return vec
}

// checkTrace asserts the two steps dense retrieval records.
func checkTrace(t *testing.T, trace []traceStep, candidates int) {
	t.Helper()
	if len(trace) != 2 {
		t.Fatalf("trace = %+v, want the embed and the search steps", trace)
	}
	if step := trace[0]; step.Step != "embed_query" || step.Technique != "dense" || step.Model != embeddingModel || step.DurationMS < 0 {
		t.Errorf("trace[0] = %+v, want the embed step naming %q", step, embeddingModel)
	}
	if step := trace[1]; step.Step != "search" || step.Technique != "dense" || step.Candidates != candidates || step.DurationMS < 0 {
		t.Errorf("trace[1] = %+v, want the search step with %d candidates", step, candidates)
	}
}

// TestRetrieveReturnsOnlyChunksOfDocumentsTheCallerCanRead is the leakage
// test for the retrieval path: Bob never receives Chunks of Alice's
// Restricted Document, even though the query vector is pinned exactly to one
// of them, and Alice does receive them, first.
func TestRetrieveReturnsOnlyChunksOfDocumentsTheCallerCanRead(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	bob := tr.principalKey(t, "bob")

	secretID := tr.upload(t, alice, "secret.md", "# Secret\n\nThe launch code is 1234.\n")
	// A Document Alice shares with Bob through its Access List.
	sharedID := tr.uploadBytesWithFields(t, alice, "shared.md",
		[]byte("# Shared\n\nThis note is for bob.\n"), map[string]string{"principals": "bob"})
	bobID := tr.upload(t, bob, "bob.md", "# Bob\n\nBob's own note.\n")

	for _, doc := range []struct{ id, key, name string }{
		{secretID, alice, "Alice's secret"},
		{sharedID, alice, "Alice's shared"},
		{bobID, bob, "Bob's own"},
	} {
		if got := tr.waitForIngest(t, doc.key, doc.id); got.ParseStatus != "completed" {
			t.Fatalf("%s Document parse_status = %q (%q), want completed", doc.name, got.ParseStatus, got.ParseError)
		}
	}

	// Pin the query to the secret's first Chunk vector: if the search ignored
	// the readable set, that Chunk would rank first for Bob too.
	vectors := tr.chunkVectors(t, secretID)
	if len(vectors) == 0 {
		t.Fatal("the secret Document has no embedded Chunk")
	}
	tr.provider.EmbedAs(retrieveQuery, vectors[0])

	bobAnswer := tr.retrieve(t, bob, retrieveQuery, "dense", 10)
	bobDocs := map[string]bool{}
	for _, result := range bobAnswer.Data.Results {
		if result.DocumentID == secretID {
			t.Errorf("bob received Chunk %s of alice's Restricted Document", result.ChunkID)
		}
		bobDocs[result.DocumentID] = true
	}
	if !bobDocs[sharedID] {
		t.Errorf("bob's results = %v, want the shared Document %s", bobDocs, sharedID)
	}
	if !bobDocs[bobID] {
		t.Errorf("bob's results = %v, want his own Document %s", bobDocs, bobID)
	}
	if len(bobAnswer.Data.Results) == 0 {
		t.Fatal("bob received no results, want the Documents he can read")
	}
	for i, result := range bobAnswer.Data.Results {
		if dense, ok := result.Scores["dense"]; !ok || math.Abs(dense-result.Score) > 1e-9 {
			t.Errorf("result %s scores = %v, score %v; want a matching dense entry", result.ChunkID, result.Scores, result.Score)
		}
		if result.Ranks["dense"] != i+1 {
			t.Errorf("result %s ranks = %v, want dense rank %d", result.ChunkID, result.Ranks, i+1)
		}
		if i > 0 && bobAnswer.Data.Results[i-1].Score < result.Score {
			t.Errorf("results out of order at %d: %v then %v", i, bobAnswer.Data.Results[i-1].Score, result.Score)
		}
	}
	checkTrace(t, bobAnswer.Data.Trace, len(bobAnswer.Data.Results))

	aliceAnswer := tr.retrieve(t, alice, retrieveQuery, "dense", 10)
	if len(aliceAnswer.Data.Results) == 0 {
		t.Fatal("alice received no results, want her own Documents")
	}
	first := aliceAnswer.Data.Results[0]
	if first.DocumentID != secretID {
		t.Errorf("alice's first result is from %s, want the secret Document %s its pinned vector ranks first",
			first.DocumentID, secretID)
	}
	if math.Abs(first.Score-1) > 1e-4 {
		t.Errorf("alice's first result score = %v, want cosine similarity 1", first.Score)
	}
	if first.Content == "" || first.ChunkID == "" || first.Scores["dense"] != first.Score {
		t.Errorf("alice's first result = %+v, want a Chunk with content and a dense score", first)
	}
	checkTrace(t, aliceAnswer.Data.Trace, len(aliceAnswer.Data.Results))

	// The query itself is what the service embedded, not a Chunk or a batch
	// of them.
	sawQuery := false
	for _, req := range tr.provider.Requests(modeltest.Embeddings) {
		var body struct {
			Input []string `json:"input"`
		}
		if json.Unmarshal(req.Body, &body) != nil {
			continue
		}
		for _, text := range body.Input {
			if text == retrieveQuery {
				sawQuery = true
			}
		}
	}
	if !sawQuery {
		t.Error("the embedding provider never saw the query text on /embeddings")
	}
}

// TestRetrieveBM25ReturnsOnlyChunksOfDocumentsTheCallerCanRead is the leakage
// test for the BM25 path, and the acceptance case for a query typed without
// diacritics: "thue tncn" must find "thuế TNCN". Alice's Restricted Document
// repeats the query syllables and scores highest, so a search that ranked
// globally and filtered afterwards would answer Bob with too few results.
func TestRetrieveBM25ReturnsOnlyChunksOfDocumentsTheCallerCanRead(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	bob := tr.principalKey(t, "bob")

	const bm25Query = "thue tncn"
	secretID := tr.upload(t, alice, "secret.md",
		"# Secret\n\nThuế TNCN thuế TNCN thuế TNCN thuế TNCN thuế TNCN là thuế thu nhập cá nhân.\n")
	// A Document Alice shares with Bob through its Access List.
	sharedID := tr.uploadBytesWithFields(t, alice, "shared.md",
		[]byte("# Shared\n\nThuế TNCN được khấu trừ tại nguồn.\n"), map[string]string{"principals": "bob"})
	bobID := tr.upload(t, bob, "bob.md", "# Bob\n\nThuế TNCN của bob.\n")

	for _, doc := range []struct{ id, key, name string }{
		{secretID, alice, "Alice's secret"},
		{sharedID, alice, "Alice's shared"},
		{bobID, bob, "Bob's own"},
	} {
		if got := tr.waitForIngest(t, doc.key, doc.id); got.ParseStatus != "completed" {
			t.Fatalf("%s Document parse_status = %q (%q), want completed", doc.name, got.ParseStatus, got.ParseError)
		}
	}

	// One result is the readable top-K: the secret's repeated syllables would
	// win the global first place and leave a post-filtered answer empty.
	bobTop := tr.retrieve(t, bob, bm25Query, "bm25", 1)
	if len(bobTop.Data.Results) != 1 {
		t.Fatalf("bob's bm25 top 1 returned %d results, want the readable top 1", len(bobTop.Data.Results))
	}
	if got := bobTop.Data.Results[0].DocumentID; got == secretID {
		t.Errorf("bob's top result is Alice's Restricted Document %s", got)
	}

	bobAnswer := tr.retrieve(t, bob, bm25Query, "bm25", 10)
	bobDocs := map[string]bool{}
	for i, result := range bobAnswer.Data.Results {
		if result.DocumentID == secretID {
			t.Errorf("bob received Chunk %s of alice's Restricted Document", result.ChunkID)
		}
		bobDocs[result.DocumentID] = true
		if bm25, ok := result.Scores["bm25"]; !ok || math.Abs(bm25-result.Score) > 1e-9 {
			t.Errorf("result %s scores = %v, score %v; want a matching bm25 entry", result.ChunkID, result.Scores, result.Score)
		}
		if result.Ranks["bm25"] != i+1 {
			t.Errorf("result %s ranks = %v, want bm25 rank %d", result.ChunkID, result.Ranks, i+1)
		}
		if _, ok := result.Scores["dense"]; ok {
			t.Errorf("result %s carries a dense score, want bm25 alone: %v", result.ChunkID, result.Scores)
		}
		if result.Content == "" || result.ChunkID == "" {
			t.Errorf("result = %+v, want a Chunk with an ID and content", result)
		}
	}
	if !bobDocs[sharedID] {
		t.Errorf("bob's results = %v, want the shared Document %s", bobDocs, sharedID)
	}
	if !bobDocs[bobID] {
		t.Errorf("bob's results = %v, want his own Document %s", bobDocs, bobID)
	}
	if len(bobAnswer.Data.Trace) != 1 {
		t.Fatalf("bm25 trace = %+v, want the search step alone", bobAnswer.Data.Trace)
	}
	if step := bobAnswer.Data.Trace[0]; step.Step != "search" || step.Technique != "bm25" ||
		step.Candidates != len(bobAnswer.Data.Results) || step.DurationMS < 0 {
		t.Errorf("bm25 trace[0] = %+v, want the search step with %d candidates", step, len(bobAnswer.Data.Results))
	}

	// A query typed with diacritics finds the same Document: the exact field
	// sees thuế and the folded field folds the query too.
	aliceAnswer := tr.retrieve(t, alice, "thuế TNCN", "bm25", 10)
	if len(aliceAnswer.Data.Results) == 0 {
		t.Fatal("alice received no bm25 results, want her own Documents")
	}
	sawSecret := false
	for _, result := range aliceAnswer.Data.Results {
		if result.DocumentID == secretID {
			sawSecret = true
		}
	}
	if !sawSecret {
		t.Errorf("alice's bm25 results = %+v, want her secret Document %s", aliceAnswer.Data.Results, secretID)
	}
	if first := aliceAnswer.Data.Results[0]; first.DocumentID != secretID {
		t.Errorf("alice's first bm25 result is from %s, want the secret Document %s its repeated syllables rank first",
			first.DocumentID, secretID)
	}

	// BM25 never sends the query to the embedding provider.
	for _, req := range tr.provider.Requests(modeltest.Embeddings) {
		var body struct {
			Input []string `json:"input"`
		}
		if json.Unmarshal(req.Body, &body) != nil {
			continue
		}
		for _, text := range body.Input {
			if text == bm25Query || text == "thuế TNCN" {
				t.Errorf("the embedding provider saw a bm25 query text %q", text)
			}
		}
	}
}

// TestRetrieveHybridReturnsOnlyChunksOfDocumentsTheCallerCanRead is the
// leakage test for RRF fusion: both lists are fetched over the caller's
// readable set, so Bob's fused answer never contains Alice's Restricted
// Document even though it ranks first in both of Alice's lists.
func TestRetrieveHybridReturnsOnlyChunksOfDocumentsTheCallerCanRead(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	bob := tr.principalKey(t, "bob")

	const query = "thue tncn"
	secretID := tr.upload(t, alice, "secret.md",
		"# Secret\n\nThuế TNCN thuế TNCN thuế TNCN thuế TNCN thuế TNCN là thuế thu nhập cá nhân.\n")
	// A Document Alice shares with Bob through its Access List.
	sharedID := tr.uploadBytesWithFields(t, alice, "shared.md",
		[]byte("# Shared\n\nThuế TNCN được khấu trừ tại nguồn.\n"), map[string]string{"principals": "bob"})
	bobID := tr.upload(t, bob, "bob.md", "# Bob\n\nThuế TNCN của bob.\n")

	for _, doc := range []struct{ id, key, name string }{
		{secretID, alice, "Alice's secret"},
		{sharedID, alice, "Alice's shared"},
		{bobID, bob, "Bob's own"},
	} {
		if got := tr.waitForIngest(t, doc.key, doc.id); got.ParseStatus != "completed" {
			t.Fatalf("%s Document parse_status = %q (%q), want completed", doc.name, got.ParseStatus, got.ParseError)
		}
	}

	// Both lists would rank the secret first for Alice: pin the dense vector
	// to its first Chunk, and its repeated syllables win BM25.
	vectors := tr.chunkVectors(t, secretID)
	if len(vectors) == 0 {
		t.Fatal("the secret Document has no embedded Chunk")
	}
	tr.provider.EmbedAs(query, vectors[0])

	const rrfK = 10
	bobAnswer := tr.retrieveConfig(t, bob, query, 10, map[string]any{
		"techniques": []string{"bm25", "dense"},
		"rrf_k":      rrfK,
	})
	if len(bobAnswer.Data.Results) == 0 {
		t.Fatal("bob received no fused results, want the Documents he can read")
	}
	bobDocs := map[string]bool{}
	rankByTechnique := map[string]map[int]bool{}
	for _, result := range bobAnswer.Data.Results {
		if result.DocumentID == secretID {
			t.Errorf("bob received Chunk %s of alice's Restricted Document", result.ChunkID)
		}
		bobDocs[result.DocumentID] = true
		// The fused score is the RRF sum over the lists the Chunk is in, and
		// every rank shown carries that technique's score.
		want := 0.0
		if len(result.Ranks) == 0 {
			t.Errorf("result %s ranks = %v, want at least one technique's rank", result.ChunkID, result.Ranks)
		}
		for technique, rank := range result.Ranks {
			if rank < 1 {
				t.Errorf("result %s %s rank = %d, want 1-based", result.ChunkID, technique, rank)
			}
			if _, ok := result.Scores[technique]; !ok {
				t.Errorf("result %s ranks %s at %d but scores = %v, want that technique's score",
					result.ChunkID, technique, rank, result.Scores)
			}
			if rankByTechnique[technique] == nil {
				rankByTechnique[technique] = map[int]bool{}
			}
			if rankByTechnique[technique][rank] {
				t.Errorf("two results share %s rank %d", technique, rank)
			}
			rankByTechnique[technique][rank] = true
			want += 1 / float64(rrfK+rank)
		}
		if math.Abs(result.Score-want) > 1e-12 {
			t.Errorf("result %s score = %v, want the RRF sum %v from ranks %v", result.ChunkID, result.Score, want, result.Ranks)
		}
	}
	if !bobDocs[sharedID] {
		t.Errorf("bob's fused results = %v, want the shared Document %s", bobDocs, sharedID)
	}
	if !bobDocs[bobID] {
		t.Errorf("bob's fused results = %v, want his own Document %s", bobDocs, bobID)
	}

	// bm25's search, dense's two steps, then one fuse step naming rrf.
	trace := bobAnswer.Data.Trace
	if len(trace) != 4 {
		t.Fatalf("hybrid trace = %+v, want bm25's search, dense's two steps and the fuse step", trace)
	}
	if step := trace[0]; step.Step != "search" || step.Technique != "bm25" || step.DurationMS < 0 {
		t.Errorf("trace[0] = %+v, want the bm25 search step", step)
	}
	if step := trace[1]; step.Step != "embed_query" || step.Technique != "dense" || step.Model != embeddingModel {
		t.Errorf("trace[1] = %+v, want the dense embed step", step)
	}
	if step := trace[2]; step.Step != "search" || step.Technique != "dense" || step.DurationMS < 0 {
		t.Errorf("trace[2] = %+v, want the dense search step", step)
	}
	if step := trace[3]; step.Step != "fuse" || step.Technique != "rrf" ||
		step.DurationMS < 0 || step.Candidates < len(bobAnswer.Data.Results) {
		t.Errorf("trace[3] = %+v, want the fuse step with at least %d candidates", step, len(bobAnswer.Data.Results))
	}

	aliceAnswer := tr.retrieveConfig(t, alice, query, 10, map[string]any{
		"techniques": []string{"bm25", "dense"},
		"rrf_k":      rrfK,
	})
	if len(aliceAnswer.Data.Results) == 0 {
		t.Fatal("alice received no fused results, want her own Documents")
	}
	if first := aliceAnswer.Data.Results[0]; first.DocumentID != secretID {
		t.Errorf("alice's first fused result is from %s, want the secret Document %s both lists rank first",
			first.DocumentID, secretID)
	}
}
