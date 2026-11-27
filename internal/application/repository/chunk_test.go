package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
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

// singleConnPool returns a migrated test database on a one-connection pool, so
// a prepared statement and the pg_prepared_statements lookup share a session.
func singleConnPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbtest.Migrated(t)
	cfg := pool.Config()
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("single-connection pool: %v", err)
	}
	t.Cleanup(single.Close)
	return single
}

// chunkSeed is one Chunk a test installs: its text and optional context
// header.
type chunkSeed struct {
	content string
	header  string
}

// replaceChunks replaces doc's Chunks with seeds in seq order and returns the
// stored rows, so a test can name their IDs.
func replaceChunks(t *testing.T, s db.Store, doc *types.Document, seeds []chunkSeed) []*types.Chunk {
	t.Helper()
	ctx := context.Background()
	repo := NewChunkRepository(s)
	input := make([]*types.Chunk, len(seeds))
	offset := 0
	for i, seed := range seeds {
		input[i] = &types.Chunk{
			Seq:           i,
			Content:       seed.content,
			ContextHeader: seed.header,
			StartOffset:   offset,
			EndOffset:     offset + len(seed.content),
		}
		offset += len(seed.content)
	}
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceChunks(ctx, doc.ID, input)
	}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}
	stored, err := repo.ListUnembeddedChunks(ctx, doc.ID, 0, len(seeds))
	if err != nil || len(stored) != len(seeds) {
		t.Fatalf("ListUnembeddedChunks = %+v, %v; want %d Chunks", stored, err, len(seeds))
	}
	return stored
}

// chunkIDs returns matches' Chunk IDs in order.
func chunkIDs(matches []*types.ChunkMatch) []uuid.UUID {
	ids := make([]uuid.UUID, len(matches))
	for i, match := range matches {
		ids[i] = match.Chunk.ID
	}
	return ids
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
		seeds := make([]chunkSeed, len(vectors))
		for i := range vectors {
			seeds[i] = chunkSeed{content: fmt.Sprintf("chunk %d", i)}
		}
		stored := replaceChunks(t, s, doc, seeds)
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

	stored := replaceChunks(t, s, doc, []chunkSeed{
		{content: "zero vector"},
		{content: "unit vector"},
	})
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

func TestSearchBM25MatchesFoldedSyllablesAndSkipsUnreadableDocuments(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	searcher := NewChunkSearcher(s)

	readable := document(t, s, ids["alice"])
	readableChunks := replaceChunks(t, s, readable, []chunkSeed{
		{header: "Chương I", content: "Người nộp thuế TNCN phải khai thuế thu nhập cá nhân."},
		{content: "Điều 1. Phạm vi điều chỉnh của luật này."},
		{content: "Thuế thu nhập cá nhân áp dụng cho cá nhân cư trú."},
		{content: "Tờ khai thuế TNCN nộp theo mẫu."},
		{content: "Thuế giá trị gia tăng."},
	})
	other := document(t, s, ids["alice"])
	otherChunks := replaceChunks(t, s, other, []chunkSeed{
		{content: "Thuế TNCN và thuế thu nhập doanh nghiệp."},
	})
	// The best match for the query, deliberately left out of the readable set.
	secret := document(t, s, ids["alice"])
	replaceChunks(t, s, secret, []chunkSeed{
		{content: "Thuế TNCN thuế TNCN thuế TNCN thuế TNCN thuế TNCN."},
	})
	tied := document(t, s, ids["alice"])
	tiedChunks := replaceChunks(t, s, tied, []chunkSeed{
		{content: "Thuế TNCN."},
		{content: "Thuế TNCN."},
	})

	documentIDs := []uuid.UUID{readable.ID, other.ID}

	t.Run("folded match and rank", func(t *testing.T) {
		got, err := searcher.SearchBM25(ctx, documentIDs, "thue tncn", 10)
		if err != nil {
			t.Fatalf("SearchBM25: %v", err)
		}
		gotIDs := chunkIDs(got)
		for i, match := range got {
			if match.Chunk.DocumentID != readable.ID && match.Chunk.DocumentID != other.ID {
				t.Errorf("match %d is from %s, want only a readable Document", i, match.Chunk.DocumentID)
			}
			if match.Score <= 0 || math.IsNaN(match.Score) {
				t.Errorf("match %d score = %v, want a positive BM25 score", i, match.Score)
			}
			if i > 0 && got[i-1].Score < match.Score {
				t.Errorf("matches out of order at %d: %v then %v", i, got[i-1].Score, match.Score)
			}
		}
		if slices.Contains(gotIDs, readableChunks[1].ID) {
			t.Errorf("the unrelated Chunk %s matched %q", readableChunks[1].ID, "thue tncn")
		}
		// The Chunk with both query syllables ranks above the one that shares
		// only "thue".
		both, shared := slices.Index(gotIDs, readableChunks[0].ID), slices.Index(gotIDs, readableChunks[4].ID)
		if both == -1 || shared == -1 {
			t.Fatalf("matches = %v, want the Chunks with thuế TNCN (%s) and thuế alone (%s)",
				gotIDs, readableChunks[0].ID, readableChunks[4].ID)
		}
		if both > shared {
			t.Errorf("the thuế TNCN Chunk ranked below the thuế-only Chunk: %v", gotIDs)
		}
	})

	t.Run("diacritic query", func(t *testing.T) {
		// A query typed with diacritics matches the same Chunks: the exact
		// field sees "thuế", the folded field folds the query too.
		accented, err := searcher.SearchBM25(ctx, documentIDs, "thuế", 10)
		if err != nil {
			t.Fatalf("SearchBM25(thuế): %v", err)
		}
		accentedIDs := chunkIDs(accented)
		if len(accentedIDs) == 0 {
			t.Fatal("SearchBM25(thuế) found nothing, want the Chunks containing thuế")
		}
		for _, id := range []uuid.UUID{readableChunks[0].ID, readableChunks[2].ID, readableChunks[4].ID, otherChunks[0].ID} {
			if !slices.Contains(accentedIDs, id) {
				t.Errorf("SearchBM25(thuế) = %v, want Chunk %s", accentedIDs, id)
			}
		}
	})

	t.Run("context header", func(t *testing.T) {
		// The context header is indexed with the content: "chuong" only
		// occurs in the first Chunk's header.
		header, err := searcher.SearchBM25(ctx, []uuid.UUID{readable.ID}, "chuong", 10)
		if err != nil {
			t.Fatalf("SearchBM25(chuong): %v", err)
		}
		if len(header) != 1 || header[0].Chunk.ID != readableChunks[0].ID {
			t.Errorf("SearchBM25(chuong) = %+v, want only the Chunk headed Chương I", header)
		}
	})

	t.Run("Access List top-K", func(t *testing.T) {
		// The secret Chunk scores highest of all, so a top-K that ranked first
		// and filtered afterwards would come back short.
		readableOnly, err := searcher.SearchBM25(ctx, []uuid.UUID{readable.ID}, "thue tncn", 2)
		if err != nil {
			t.Fatalf("SearchBM25(readable only): %v", err)
		}
		if len(readableOnly) != 2 {
			t.Fatalf("SearchBM25(readable only, top 2) returned %d matches, want the readable top 2", len(readableOnly))
		}
		for _, match := range readableOnly {
			if match.Chunk.DocumentID != readable.ID {
				t.Errorf("readable-only search returned a Chunk of %s", match.Chunk.DocumentID)
			}
		}
	})

	t.Run("punctuation", func(t *testing.T) {
		// Query punctuation is only a token separator, never the Tantivy query
		// grammar, so it cannot error or change which Chunks match.
		plain, err := searcher.SearchBM25(ctx, documentIDs, "thue tncn", 10)
		if err != nil {
			t.Fatalf("SearchBM25(plain): %v", err)
		}
		special, err := searcher.SearchBM25(ctx, documentIDs, `thue: (tncn) "AND" - *`, 10)
		if err != nil {
			t.Fatalf("SearchBM25(special characters): %v", err)
		}
		plainIDs, specialIDs := chunkIDs(plain), chunkIDs(special)
		if !slices.Equal(specialIDs, plainIDs) {
			t.Errorf("results with query punctuation = %v, want the same Chunks as %q: %v",
				specialIDs, "thue tncn", plainIDs)
		}
	})

	t.Run("empty answers", func(t *testing.T) {
		// No matching token, or a query with no tokens at all, is an empty
		// answer, not an error. An empty readable set is one too.
		for _, query := range []string{"zzzz qqqq", ":"} {
			none, err := searcher.SearchBM25(ctx, documentIDs, query, 10)
			if err != nil || none == nil || len(none) != 0 {
				t.Errorf("SearchBM25(%q) = %+v, %v; want an empty non-nil slice", query, none, err)
			}
		}
		if none, err := searcher.SearchBM25(ctx, nil, "thue tncn", 10); err != nil || len(none) != 0 {
			t.Errorf("SearchBM25(no readable Documents) = %+v, %v; want nothing", none, err)
		}
	})

	t.Run("ties ordered by Chunk ID", func(t *testing.T) {
		tie, err := searcher.SearchBM25(ctx, []uuid.UUID{tied.ID}, "thue tncn", 10)
		if err != nil {
			t.Fatalf("SearchBM25(ties): %v", err)
		}
		if len(tie) != 2 || tie[0].Score != tie[1].Score {
			t.Fatalf("SearchBM25(ties) = %+v, want two Chunks with the same score", tie)
		}
		want := []uuid.UUID{tiedChunks[0].ID, tiedChunks[1].ID}
		slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		if !slices.Equal(chunkIDs(tie), want) {
			t.Errorf("ties ordered %v, want ascending Chunk ID %v", chunkIDs(tie), want)
		}
	})
}

// preFixSearchQuery is the pre-#143 dense search: it parses the text query
// vector once for every Chunk. The #143 test explains it as a control; the
// #139 test runs it to force the bad qual order.
const preFixSearchQuery = `SELECT id, document_id, seq, content, context_header,
       (1 - (embedding <=> $1::text::vector))::float8 AS score
FROM chunks
WHERE document_id = ANY($2::uuid[])
  AND embedding_model = $3::text
  AND embedding IS NOT NULL
  AND (embedding <=> $1::text::vector) <> 'NaN'::float8
ORDER BY embedding <=> $1::text::vector, id
LIMIT $4`

// TestSearchDenseSkipsChunksOfAnotherModelAtAnotherDimension pins #139: a
// Chunk embedded by another model, at another dimension, must not make the
// search error whichever qual the planner evaluates first. The distance is
// guarded by a CASE, so it is only computed for Chunks of the searched model.
func TestSearchDenseSkipsChunksOfAnotherModelAtAnotherDimension(t *testing.T) {
	ctx := context.Background()
	single := singleConnPool(t)

	s := db.NewStore(single)
	owner, err := s.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := document(t, s, owner.ID)
	chunks := NewChunkRepository(s)
	searcher := NewChunkSearcher(s)

	seed := []struct {
		content string
		vector  []float32
		model   string
	}{
		{"first", []float32{1, 0, 0}, "test-embedding"},
		{"another model", []float32{1, 0, 0, 0}, "other-embedding"},
		{"second", []float32{0, 1, 0}, "test-embedding"},
	}
	input := make([]*types.Chunk, len(seed))
	offset := 0
	for i, row := range seed {
		input[i] = &types.Chunk{
			Seq:         i,
			Content:     row.content,
			StartOffset: offset,
			EndOffset:   offset + len(row.content),
		}
		offset += len(row.content)
	}
	if err := s.ExecTx(ctx, func(ctx context.Context) error {
		return chunks.ReplaceChunks(ctx, doc.ID, input)
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := chunks.ListUnembeddedChunks(ctx, doc.ID, 0, len(seed))
	if err != nil || len(stored) != len(seed) {
		t.Fatalf("ListUnembeddedChunks = %+v, %v; want %d Chunks", stored, err, len(seed))
	}
	for i, c := range stored {
		if err := chunks.SetChunkEmbedding(ctx, c.ID, seed[i].vector, seed[i].model); err != nil {
			t.Fatal(err)
		}
	}

	// Make the whole distance comparison cheaper than the model comparison,
	// so the planner evaluates it first. The control below then proves the
	// order with an unguarded query; the repository query must stay safe.
	for _, stmt := range []string{
		`ALTER FUNCTION public.cosine_distance(vector, vector) COST 0.0001`,
		`ALTER FUNCTION pg_catalog.float8ne(float8, float8) COST 0.0001`,
	} {
		if _, err := single.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// The pre-#143 shape keeps the distance in the scan filter next to the
	// model check. Under the forced order it computes the 3-dim query
	// distance on the 4-dim Chunk and errors.
	rows, err := single.Query(ctx, preFixSearchQuery, "[1,0,0]", []uuid.UUID{doc.ID}, "test-embedding", 10)
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "different vector dimensions") {
		t.Fatalf("the unguarded control query = %v, want a dimension error; the forced qual order no longer holds", err)
	}

	got, err := searcher.SearchDense(ctx, []uuid.UUID{doc.ID}, []float32{1, 0, 0}, "test-embedding", 10)
	if err != nil {
		t.Fatalf("SearchDense: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("SearchDense returned %d matches, want only the 2 Chunks of the searched model", len(got))
	}
	if got[0].Chunk.ID != stored[0].ID || got[1].Chunk.ID != stored[2].ID {
		t.Errorf("SearchDense returned seq %d then %d, want seq 0 then seq 2",
			got[0].Chunk.Seq, got[1].Chunk.Seq)
	}
	if math.Abs(got[0].Score-1) > 1e-5 || math.Abs(got[1].Score) > 1e-5 {
		t.Errorf("scores = %v, %v; want 1 then 0", got[0].Score, got[1].Score)
	}

	// #143's materialized query_vec makes the distance a join filter, after
	// the scan's model filter, so the old query happens to pass above. The
	// plan must still guard every distance by the model: another plan shape
	// may evaluate it in the scan filter, as the control just did.
	plan := explainGeneric(t, ctx, single, preparedStatementText(t, ctx, single, "SearchDenseChunks"))
	if expr := unguardedDistance(plan); expr != "" {
		t.Errorf("SearchDenseChunks computes a distance without checking the Chunk's model first: %s; #139", expr)
	}
}

// TestSearchDenseCastsTheQueryVectorOnceUnderAGenericPlan pins the #143
// mechanism: a generic plan for the old query cast the text query vector once
// for every Chunk it scanned. The control asserts the old shape still has
// that per-Chunk cast; the repository query must cast the vector only once.
func TestSearchDenseCastsTheQueryVectorOnceUnderAGenericPlan(t *testing.T) {
	ctx := context.Background()
	single := singleConnPool(t)

	oldPlan := explainGeneric(t, ctx, single, preFixSearchQuery)
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

// guardedDistance matches a cosine distance in the THEN branch of a CASE that
// checks the Chunk's model, as EXPLAIN prints it.
var guardedDistance = regexp.MustCompile(`CASE WHEN \(chunks\.embedding_model = [^)]*\) THEN \(chunks\.embedding <=> `)

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

// TestGetReadableChunkReturnsOnlyAReadableDocumentsChunk is the access case
// for the Chunk read endpoint: a reader gets the Chunk of a Document shared
// with them, while a Chunk of another Document is the same not-found error as
// an unknown ID (ADR-0004).
func TestGetReadableChunkReturnsOnlyAReadableDocumentsChunk(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	reader := NewChunkReader(s)

	shared := documentGranted(t, s, ids["alice"], types.AccessGrants{PrincipalIDs: []uuid.UUID{ids["bob"]}})
	sharedChunks := replaceChunks(t, s, shared, []chunkSeed{{header: "Chương I", content: "Điều 1. Phạm vi điều chỉnh."}})
	secret := document(t, s, ids["alice"])
	secretChunks := replaceChunks(t, s, secret, []chunkSeed{{content: "Mã ra mắt là 1234."}})

	got, err := reader.GetReadableChunk(ctx, ids["bob"], sharedChunks[0].ID)
	if err != nil {
		t.Fatalf("GetReadableChunk(shared): %v", err)
	}
	if got.ID != sharedChunks[0].ID || got.DocumentID != shared.ID || got.Seq != 0 ||
		got.Content != "Điều 1. Phạm vi điều chỉnh." || got.ContextHeader != "Chương I" ||
		got.StartOffset != sharedChunks[0].StartOffset || got.EndOffset != sharedChunks[0].EndOffset {
		t.Errorf("chunk = %+v, want the stored shared Chunk %+v", got, sharedChunks[0])
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at = zero, want the stored creation time")
	}

	// The owner still reads her own Document's Chunk.
	if _, err := reader.GetReadableChunk(ctx, ids["alice"], secretChunks[0].ID); err != nil {
		t.Errorf("GetReadableChunk(own Document) = %v, want the Chunk", err)
	}
	// A Chunk of a Document Bob cannot read and an unknown ID are the same
	// not-found error, so the answer reveals nothing about what exists.
	for name, id := range map[string]uuid.UUID{
		"restricted Document": secretChunks[0].ID,
		"unknown Chunk":       uuid.New(),
	} {
		if _, err := reader.GetReadableChunk(ctx, ids["bob"], id); !errors.Is(err, ErrChunkNotFound) {
			t.Errorf("GetReadableChunk(%s) = %v, want ErrChunkNotFound", name, err)
		}
	}
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

// unguardedDistance returns the first plan expression with a cosine distance
// outside the THEN branch of a CASE that checks the Chunk's model, or "" when
// every distance is guarded. That guard is what keeps a plan that evaluates
// the distance before the model filter safe (#139).
func unguardedDistance(node planNode) string {
	for _, expr := range node.expressions() {
		if distances := strings.Count(expr, "<=>"); distances > 0 &&
			len(guardedDistance.FindAllString(expr, -1)) != distances {
			return expr
		}
	}
	for _, child := range node.Plans {
		if expr := unguardedDistance(child); expr != "" {
			return expr
		}
	}
	return ""
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
