//go:build integration && anydoc

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// chunkEmbedding is what ingest stored on one Chunk: the vector's length and
// the model that produced it.
type chunkEmbedding struct {
	Dims  int
	Model string
}

// chunkEmbeddings returns this Trelis's stored embeddings for a Document in
// seq order, read straight from Postgres. A Chunk without an embedding comes
// back with zero dimensions and no model, so a test can see the hole.
func (tr *trelis) chunkEmbeddings(t *testing.T, documentID string) []chunkEmbedding {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, deps.pg.host, deps.pg.port, tr.dbname))
	if err != nil {
		t.Fatalf("connect to database %s: %v", tr.dbname, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, `SELECT coalesce(vector_dims(embedding), 0), coalesce(embedding_model, '')
FROM chunks WHERE document_id = $1 ORDER BY seq`, documentID)
	if err != nil {
		t.Fatalf("select embeddings of %s: %v", documentID, err)
	}
	defer rows.Close()
	var embeddings []chunkEmbedding
	for rows.Next() {
		var e chunkEmbedding
		if err := rows.Scan(&e.Dims, &e.Model); err != nil {
			t.Fatal(err)
		}
		embeddings = append(embeddings, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return embeddings
}

// uploadTextDocx uploads the sample docx the parser tests use and returns the
// new Document's ID. The real anydoc engine must be linked, which the build
// tag on this file guarantees.
func uploadTextDocx(t *testing.T, tr *trelis, key string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "parser", "anydoc", "testdata", "upstream", "docx", "text.docx"))
	if err != nil {
		t.Fatal(err)
	}
	return tr.uploadBytes(t, key, "text.docx", data)
}

func TestIngestEmbedsEveryChunk(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	id := uploadTextDocx(t, tr, alice)

	doc := tr.waitForIngest(t, alice, id)
	if doc.ParseStatus != "completed" {
		t.Fatalf("parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}
	chunks := tr.chunkContents(t, id)
	embeddings := tr.chunkEmbeddings(t, id)
	if len(chunks) == 0 {
		t.Fatal("no Chunks stored for a parsed docx")
	}
	if len(embeddings) != len(chunks) {
		t.Fatalf("embeddings = %d for %d Chunks, want one per Chunk", len(embeddings), len(chunks))
	}
	for i, e := range embeddings {
		if e.Dims != modeltest.DefaultEmbeddingDims || e.Model != embeddingModel {
			t.Errorf("chunk %d embedding = %d dimensions by %q, want %d by %q",
				i, e.Dims, e.Model, modeltest.DefaultEmbeddingDims, embeddingModel)
		}
	}
}

func TestATransientEmbeddingFailureStillCompletesTheDocument(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	// More failures than the embedding client retries inside one activity
	// attempt, so the first attempt fails and Temporal retries the activity.
	// The sample's Chunks fit in one batch, so the retry redoes only that batch,
	// and the workflow test proves it did not re-parse the Document.
	failures := api.DefaultRetry().MaxAttempts + 1
	tr.provider.FailNext(modeltest.Embeddings, http.StatusServiceUnavailable, failures)
	id := uploadTextDocx(t, tr, alice)

	doc := tr.waitForIngest(t, alice, id)
	if doc.ParseStatus != "completed" {
		t.Fatalf("parse_status = %q (%q) after %d embedding failures, want completed",
			doc.ParseStatus, doc.ParseError, failures)
	}
	embeddings := tr.chunkEmbeddings(t, id)
	if len(embeddings) == 0 {
		t.Fatal("no embeddings stored after the retried embed")
	}
	for i, e := range embeddings {
		if e.Dims != modeltest.DefaultEmbeddingDims {
			t.Errorf("chunk %d embedding has %d dimensions, want %d", i, e.Dims, modeltest.DefaultEmbeddingDims)
		}
	}
}

func TestIngestWithoutAnEmbeddingModelFailsTheDocument(t *testing.T) {
	tr := startWith(t,
		"TRELIS_MODELS_EMBEDDING_BASE_URL=",
		"TRELIS_MODELS_EMBEDDING_MODEL=")
	alice := tr.principalKey(t, "alice")
	id := uploadTextDocx(t, tr, alice)

	doc := tr.waitForIngest(t, alice, id)
	if doc.ParseStatus != "failed" {
		t.Fatalf("parse_status = %q, want failed", doc.ParseStatus)
	}
	if doc.ParseError != "no embedding model is configured" {
		t.Errorf("parse_error = %q, want %q", doc.ParseError, "no embedding model is configured")
	}
	// Parsing succeeded before embedding failed, so the Chunks exist without
	// vectors.
	chunks := tr.chunkContents(t, id)
	if len(chunks) == 0 {
		t.Error("no Chunks stored, want the parsed Chunks kept despite the failed embed")
	}
	embeddings := tr.chunkEmbeddings(t, id)
	if len(embeddings) != len(chunks) {
		t.Fatalf("embeddings = %d for %d Chunks, want the Chunks read back", len(embeddings), len(chunks))
	}
	for i, e := range embeddings {
		if e.Dims != 0 || e.Model != "" {
			t.Errorf("chunk %d embedding = %d dimensions by %q, want none without an embedding model", i, e.Dims, e.Model)
		}
	}
}
