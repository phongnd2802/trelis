package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/types"
)

// document creates one pending Document owned by ownerID.
func document(t *testing.T, s db.Store, ownerID uuid.UUID) *types.Document {
	t.Helper()
	return documentGranted(t, s, ownerID, types.AccessGrants{})
}

// documentGranted creates one pending Document owned by ownerID and granted
// to the Principals and Groups in grants.
func documentGranted(t *testing.T, s db.Store, ownerID uuid.UUID, grants types.AccessGrants) *types.Document {
	t.Helper()
	doc := &types.Document{
		ID:        uuid.New(),
		OwnerID:   ownerID,
		Title:     "luat-thue.txt",
		FileName:  "luat-thue.txt",
		FileType:  "txt",
		FileSize:  1,
		FileHash:  uuid.NewString(),
		UploadID:  uuid.New(),
		ObjectKey: "documents/" + uuid.NewString() + "/source",
	}
	if err := NewDocumentRepository(s).CreateDocument(context.Background(), doc, grants); err != nil {
		t.Fatalf("create document: %v", err)
	}
	return doc
}

func TestReplaceChunksLeavesTheSameSetOnEveryRun(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	doc := document(t, s, ids["alice"])
	repo := NewChunkRepository(s)

	first := []*types.Chunk{
		{Seq: 0, Content: "Điều 1.", StartOffset: 0, EndOffset: 7},
		{Seq: 1, Content: "Điều 2.", ContextHeader: "Chương I", StartOffset: 8, EndOffset: 15},
	}
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceChunks(ctx, doc.ID, first)
	}); err != nil {
		t.Fatalf("first ReplaceChunks: %v", err)
	}
	// Running the same input again, as a retried activity would, must not
	// duplicate the rows.
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceChunks(ctx, doc.ID, first)
	}); err != nil {
		t.Fatalf("second ReplaceChunks: %v", err)
	}
	got, err := repo.ListUnembeddedChunks(ctx, doc.ID, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 0 || got[0].Content != "Điều 1." ||
		got[1].Seq != 1 || got[1].ContextHeader != "Chương I" ||
		got[0].StartOffset != 0 || got[0].EndOffset != 7 || got[1].StartOffset != 8 || got[1].EndOffset != 15 {
		t.Errorf("chunks after two identical runs = %+v, want the two inserted once", got)
	}

	// A new set replaces the old one rather than appending to it.
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceChunks(ctx, doc.ID, []*types.Chunk{{Seq: 0, Content: "Điều 3.", StartOffset: 16, EndOffset: 23}})
	}); err != nil {
		t.Fatalf("third ReplaceChunks: %v", err)
	}
	got, err = repo.ListUnembeddedChunks(ctx, doc.ID, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "Điều 3." {
		t.Errorf("chunks after a replacement = %+v, want only the new Chunk", got)
	}
}

func TestSetChunkEmbeddingStoresTheVectorAndModel(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	s := db.NewStore(pool)
	owner, err := s.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := document(t, s, owner.ID)
	repo := NewChunkRepository(s)
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceChunks(ctx, doc.ID, []*types.Chunk{
			{Seq: 0, Content: "Điều 1.", StartOffset: 0, EndOffset: 7},
			{Seq: 1, Content: "Điều 2.", ContextHeader: "Chương I", StartOffset: 8, EndOffset: 15},
		})
	}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}

	chunks, err := repo.ListUnembeddedChunks(ctx, doc.ID, 0, 2)
	if err != nil {
		t.Fatalf("ListUnembeddedChunks: %v", err)
	}
	if len(chunks) != 2 || chunks[0].ID == uuid.Nil || chunks[0].Seq != 0 || chunks[0].Content != "Điều 1." ||
		chunks[1].ContextHeader != "Chương I" || chunks[1].StartOffset != 8 || chunks[1].EndOffset != 15 {
		t.Fatalf("ListUnembeddedChunks = %+v, want the two Chunks in seq order", chunks)
	}
	if count, err := repo.CountChunksWithoutEmbedding(ctx, doc.ID); err != nil || count != 2 {
		t.Fatalf("CountChunksWithoutEmbedding = %d, %v; want 2", count, err)
	}

	if err := repo.SetChunkEmbedding(ctx, chunks[0].ID, []float32{0.5, -1.25, 3}, "test-embedding"); err != nil {
		t.Fatalf("SetChunkEmbedding: %v", err)
	}
	var vector, model string
	if err := pool.QueryRow(ctx, "SELECT embedding::text, embedding_model FROM chunks WHERE id = $1", chunks[0].ID).
		Scan(&vector, &model); err != nil {
		t.Fatal(err)
	}
	if vector != "[0.5,-1.25,3]" || model != "test-embedding" {
		t.Errorf("stored embedding = %q by %q, want [0.5,-1.25,3] by test-embedding", vector, model)
	}
	var embedded bool
	if err := pool.QueryRow(ctx, "SELECT embedding IS NOT NULL FROM chunks WHERE id = $1", chunks[1].ID).Scan(&embedded); err != nil {
		t.Fatal(err)
	}
	if embedded {
		t.Error("the second Chunk has an embedding, want it left NULL")
	}

	// An already embedded range is a no-op, and the count sees the rest.
	if got, err := repo.ListUnembeddedChunks(ctx, doc.ID, 0, 1); err != nil || len(got) != 0 {
		t.Errorf("ListUnembeddedChunks(0, 1) = %+v, %v; want nothing already embedded", got, err)
	}
	remaining, err := repo.ListUnembeddedChunks(ctx, doc.ID, 0, 2)
	if err != nil || len(remaining) != 1 || remaining[0].ID != chunks[1].ID {
		t.Fatalf("ListUnembeddedChunks after embedding one = %+v, %v; want only the second Chunk", remaining, err)
	}
	if count, err := repo.CountChunksWithoutEmbedding(ctx, doc.ID); err != nil || count != 1 {
		t.Errorf("CountChunksWithoutEmbedding = %d, %v; want 1", count, err)
	}

	// A Chunk that disappeared between listing and updating is reported, not
	// skipped: the activity then retries against the set it has now.
	if err := repo.SetChunkEmbedding(ctx, uuid.New(), []float32{1}, "test-embedding"); err == nil {
		t.Error("SetChunkEmbedding(unknown chunk) = nil, want an error")
	}
}

func TestSearchDenseOrdersByCosineAndSkipsUncomparableChunks(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	chunks := NewChunkRepository(s)
	searcher := NewChunkSearcher(s)

	visible := document(t, s, ids["alice"])
	otherModel := document(t, s, ids["alice"])
	unembedded := document(t, s, ids["alice"])
	outside := document(t, s, ids["alice"])

	replaceAndEmbed := func(doc *types.Document, vectors [][]float32, model string) []*types.Chunk {
		t.Helper()
		input := make([]*types.Chunk, len(vectors))
		for i := range vectors {
			input[i] = &types.Chunk{Seq: i, Content: fmt.Sprintf("chunk %d", i), StartOffset: i, EndOffset: i + 1}
		}
		if err := s.ExecTx(ctx, func(ctx context.Context) error {
			return chunks.ReplaceChunks(ctx, doc.ID, input)
		}); err != nil {
			t.Fatalf("ReplaceChunks: %v", err)
		}
		stored, err := chunks.ListUnembeddedChunks(ctx, doc.ID, 0, len(input))
		if err != nil || len(stored) != len(input) {
			t.Fatalf("ListUnembeddedChunks = %+v, %v; want %d Chunks", stored, err, len(input))
		}
		for i, c := range stored {
			if err := chunks.SetChunkEmbedding(ctx, c.ID, vectors[i], model); err != nil {
				t.Fatalf("SetChunkEmbedding: %v", err)
			}
		}
		return stored
	}

	stored := replaceAndEmbed(visible, [][]float32{{1, 0}, {0, 1}, {-1, 0}, {1, 0}}, "test-embedding")
	replaceAndEmbed(otherModel, [][]float32{{1, 0}}, "other-embedding")
	// outside is embedded but never in the readable set passed to the search.
	replaceAndEmbed(outside, [][]float32{{1, 0}}, "test-embedding")
	// unembedded keeps its Chunk but no vector.
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return chunks.ReplaceChunks(ctx, unembedded.ID,
			[]*types.Chunk{{Seq: 0, Content: "no vector", StartOffset: 0, EndOffset: 9}})
	}); err != nil {
		t.Fatal(err)
	}

	documentIDs := []uuid.UUID{visible.ID, otherModel.ID, unembedded.ID}
	got, err := searcher.SearchDense(ctx, documentIDs, []float32{1, 0}, "test-embedding", 10)
	if err != nil {
		t.Fatalf("SearchDense: %v", err)
	}
	if len(got) != len(stored) {
		t.Fatalf("SearchDense returned %d matches, want only the %d comparable Chunks", len(got), len(stored))
	}
	storedByID := make(map[uuid.UUID]*types.Chunk, len(stored))
	for _, c := range stored {
		storedByID[c.ID] = c
	}
	wantScores := []float64{1, 1, 0, -1}
	for i, match := range got {
		if match.Chunk.DocumentID != visible.ID {
			t.Errorf("match %d belongs to %s, want only %s", i, match.Chunk.DocumentID, visible.ID)
		}
		if math.Abs(match.Score-wantScores[i]) > 1e-5 {
			t.Errorf("match %d score = %v, want %v", i, match.Score, wantScores[i])
		}
		if want, ok := storedByID[match.Chunk.ID]; !ok || match.Chunk.Content != want.Content || match.Chunk.Seq != want.Seq {
			t.Errorf("match %d Chunk = %+v, want the stored content and seq for %s", i, match.Chunk, match.Chunk.ID)
		}
	}
	if got[0].Chunk.ID.String() >= got[1].Chunk.ID.String() {
		t.Errorf("ties ordered %s then %s, want ascending Chunk ID", got[0].Chunk.ID, got[1].Chunk.ID)
	}

	top, err := searcher.SearchDense(ctx, documentIDs, []float32{1, 0}, "test-embedding", 2)
	if err != nil || len(top) != 2 || math.Abs(top[0].Score-1) > 1e-5 || math.Abs(top[1].Score-1) > 1e-5 {
		t.Errorf("top_k 2 = %+v, %v; want the two closest Chunks", top, err)
	}

	other, err := searcher.SearchDense(ctx, documentIDs, []float32{1, 0}, "other-embedding", 10)
	if err != nil || len(other) != 1 || other[0].Chunk.DocumentID != otherModel.ID {
		t.Errorf("search for other-embedding = %+v, %v; want only its own Chunk", other, err)
	}

	none, err := searcher.SearchDense(ctx, nil, []float32{1, 0}, "test-embedding", 10)
	if err != nil || len(none) != 0 {
		t.Errorf("search over no documents = %+v, %v; want nothing", none, err)
	}
}

func TestSearchDenseSkipsNaNScoresFromZeroVectors(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	chunks := NewChunkRepository(s)
	searcher := NewChunkSearcher(s)
	doc := document(t, s, ids["alice"])

	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return chunks.ReplaceChunks(ctx, doc.ID, []*types.Chunk{
			{Seq: 0, Content: "zero vector", StartOffset: 0, EndOffset: 11},
			{Seq: 1, Content: "unit vector", StartOffset: 12, EndOffset: 23},
		})
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := chunks.ListUnembeddedChunks(ctx, doc.ID, 0, 2)
	if err != nil || len(stored) != 2 {
		t.Fatalf("ListUnembeddedChunks = %+v, %v; want both Chunks", stored, err)
	}
	// Cosine similarity is NaN when either vector has zero norm.
	if err := chunks.SetChunkEmbedding(ctx, stored[0].ID, []float32{0, 0}, "test-embedding"); err != nil {
		t.Fatal(err)
	}
	if err := chunks.SetChunkEmbedding(ctx, stored[1].ID, []float32{1, 0}, "test-embedding"); err != nil {
		t.Fatal(err)
	}

	got, err := searcher.SearchDense(ctx, []uuid.UUID{doc.ID}, []float32{1, 0}, "test-embedding", 10)
	if err != nil {
		t.Fatalf("SearchDense: %v", err)
	}
	if len(got) != 1 || got[0].Chunk.ID != stored[1].ID {
		t.Fatalf("SearchDense = %+v, want only the unit-vector Chunk", got)
	}
	if math.IsNaN(got[0].Score) {
		t.Error("the returned score is NaN")
	}

	// A zero query vector makes every distance NaN, so nothing can match.
	none, err := searcher.SearchDense(ctx, []uuid.UUID{doc.ID}, []float32{0, 0}, "test-embedding", 10)
	if err != nil || len(none) != 0 {
		t.Errorf("SearchDense with a zero query = %+v, %v; want nothing", none, err)
	}
}

// TestSearchDenseCastsTheQueryVectorOnceUnderAGenericPlan pins the #143
// mechanism: a generic plan for the old query cast the text query vector once
// for every Chunk it scanned. The control asserts the old shape still has
// that per-Chunk cast; the repository query must cast the vector only once.
func TestSearchDenseCastsTheQueryVectorOnceUnderAGenericPlan(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)

	// One connection keeps the SearchDense prepared statement and the
	// pg_prepared_statements lookup on the same session.
	cfg := pool.Config()
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("single-connection pool: %v", err)
	}
	t.Cleanup(single.Close)

	const oldQuery = `SELECT id, document_id, seq, content, context_header,
       (1 - (embedding <=> $1::text::vector))::float8 AS score
FROM chunks
WHERE document_id = ANY($2::uuid[])
  AND embedding_model = $3::text
  AND embedding IS NOT NULL
  AND (embedding <=> $1::text::vector) <> 'NaN'::float8
ORDER BY embedding <=> $1::text::vector, id
LIMIT $4`
	oldPlan := explainGeneric(t, ctx, single, oldQuery)
	if !hasPerChunkCast(oldPlan) {
		t.Fatalf("the pre-#143 query no longer casts its query vector per Chunk over chunks (%s); the old plan shape changed (for example after a Postgres upgrade), so re-check that this test still proves #143", planString(oldPlan))
	}

	// No seed: the cast is an expression in the generic plan, not a
	// data-dependent choice, so an empty chunks table is enough.
	searcher := NewChunkSearcher(db.NewStore(single))
	if _, err := searcher.SearchDense(ctx, []uuid.UUID{uuid.New()}, []float32{1, 0, 0}, "test-embedding", 10); err != nil {
		t.Fatalf("SearchDense: %v", err)
	}
	newPlan := explainGeneric(t, ctx, single, preparedStatementText(t, ctx, single, "SearchDenseChunks"))
	if hasPerChunkCast(newPlan) {
		t.Errorf("SearchDenseChunks casts its query vector per Chunk over chunks: %s; #143", planString(newPlan))
	}
}

type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	Filter       string     `json:"Filter"`
	SortKey      []string   `json:"Sort Key"`
	Output       []string   `json:"Output"`
	Plans        []planNode `json:"Plans"`
}

var vectorCast = regexp.MustCompile(`\(\$\d+\)::vector`)

// explainGeneric plans query as a generic plan, without arguments or
// execution. Postgres 16+ supports GENERIC_PLAN.
func explainGeneric(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) planNode {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire for explain: %v", err)
	}
	defer conn.Release()
	// PgConn.Exec sends a simple query, keeping the $n placeholders inside
	// the explained query instead of making them parameters of EXPLAIN.
	results, err := conn.Conn().PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON) "+query).ReadAll()
	if err != nil {
		t.Fatalf("explain generic plan: %v", err)
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 {
		t.Fatalf("explain generic plan: want one JSON row, got %d results", len(results))
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(results[0].Rows[0][0], &plans); err != nil {
		t.Fatalf("decode explain: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("explain returned %d plans, want one", len(plans))
	}
	return plans[0].Plan
}

// preparedStatementText returns the SQL the repository prepared for queryName.
// The lookup relies on sqlc's `-- name: SearchDenseChunks :many` comment
// being part of the statement text.
func preparedStatementText(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queryName string) string {
	t.Helper()
	var statement string
	if err := pool.QueryRow(ctx, `SELECT statement FROM pg_prepared_statements
		WHERE statement LIKE '%' || $1 || '%'
		ORDER BY prepare_time DESC LIMIT 1`, queryName).Scan(&statement); err != nil {
		t.Fatalf("find the prepared statement for %s: %v", queryName, err)
	}
	return statement
}

// hasPerChunkCast reports whether an expression over chunks casts the query
// vector to vector, which means the cast is evaluated per Chunk. It checks
// the chunks scan and its ancestors; the cast-once CTE's Result is a sibling.
func hasPerChunkCast(node planNode) bool {
	if overChunks(node) {
		for _, expr := range node.expressions() {
			if vectorCast.MatchString(expr) {
				return true
			}
		}
	}
	for _, child := range node.Plans {
		if hasPerChunkCast(child) {
			return true
		}
	}
	return false
}

// overChunks reports whether node or a descendant scans chunks.
func overChunks(node planNode) bool {
	if node.RelationName == "chunks" {
		return true
	}
	for _, child := range node.Plans {
		if overChunks(child) {
			return true
		}
	}
	return false
}

func (n planNode) expressions() []string {
	exprs := []string{n.Filter}
	exprs = append(exprs, n.SortKey...)
	exprs = append(exprs, n.Output...)
	return exprs
}

func planString(node planNode) string {
	parts := []string{node.NodeType}
	if node.RelationName != "" {
		parts = append(parts, "on "+node.RelationName)
	}
	for _, child := range node.Plans {
		parts = append(parts, planString(child))
	}
	return strings.Join(parts, " > ")
}

func TestSetParseStatusKeepsAFailureReasonOnlyWhileFailed(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	doc := document(t, s, ids["alice"])
	repo := NewDocumentRepository(s)

	if err := repo.SetParseStatus(ctx, doc.ID, types.ParseStatusProcessing, ""); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParseStatus != types.ParseStatusProcessing || got.ParseError != "" {
		t.Errorf("processing Document = %q/%q, want processing with no error", got.ParseStatus, got.ParseError)
	}

	if err := repo.SetParseStatus(ctx, doc.ID, types.ParseStatusFailed, "the Source File could not be parsed"); err != nil {
		t.Fatal(err)
	}
	got, err = repo.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParseStatus != types.ParseStatusFailed || got.ParseError != "the Source File could not be parsed" {
		t.Errorf("failed Document = %q/%q, want failed with its reason", got.ParseStatus, got.ParseError)
	}

	if err := repo.SetParseStatus(ctx, doc.ID, types.ParseStatusCompleted, ""); err != nil {
		t.Fatal(err)
	}
	got, err = repo.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParseStatus != types.ParseStatusCompleted || got.ParseError != "" {
		t.Errorf("completed Document = %q/%q, want completed with the reason cleared", got.ParseStatus, got.ParseError)
	}

	if _, err := repo.GetDocumentByID(ctx, uuid.New()); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("GetDocumentByID(unknown) = %v, want ErrDocumentNotFound", err)
	}
	if err := repo.SetParseStatus(ctx, uuid.New(), types.ParseStatusCompleted, ""); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("SetParseStatus(unknown) = %v, want ErrDocumentNotFound", err)
	}
}
