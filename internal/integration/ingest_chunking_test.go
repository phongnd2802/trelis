//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// storedChunk is one chunk row the ingest tests read straight from Postgres.
type storedChunk struct {
	id       string
	seq      int
	content  string
	header   string
	start    int
	parentID *uuid.UUID
}

// chunks returns this Trelis's stored Chunks for a Document in seq order,
// with their context headers and Parent Chunk links.
func (tr *trelis) chunks(t *testing.T, documentID string) []storedChunk {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, deps.pg.host, deps.pg.port, tr.dbname))
	if err != nil {
		t.Fatalf("connect to database %s: %v", tr.dbname, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, `SELECT id, seq, content, context_header, start_offset, parent_id
FROM chunks WHERE document_id = $1 ORDER BY seq`, documentID)
	if err != nil {
		t.Fatalf("select chunks of %s: %v", documentID, err)
	}
	defer rows.Close()
	var chunks []storedChunk
	for rows.Next() {
		var c storedChunk
		var parentID pgtype.UUID
		if err := rows.Scan(&c.id, &c.seq, &c.content, &c.header, &c.start, &parentID); err != nil {
			t.Fatal(err)
		}
		if parentID.Valid {
			id := uuid.UUID(parentID.Bytes)
			c.parentID = &id
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return chunks
}

// parentChunkCount is how many Parent Chunks a Document has.
func (tr *trelis) parentChunkCount(t *testing.T, documentID string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, deps.pg.host, deps.pg.port, tr.dbname))
	if err != nil {
		t.Fatalf("connect to database %s: %v", tr.dbname, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM parent_chunks WHERE document_id = $1`, documentID).Scan(&count); err != nil {
		t.Fatalf("count Parent Chunks of %s: %v", documentID, err)
	}
	return count
}

// embeddedTexts returns every text sent to the embeddings endpoint, in
// request order. The fake provider records the raw bodies.
func (tr *trelis) embeddedTexts(t *testing.T) []string {
	t.Helper()
	var texts []string
	for _, req := range tr.provider.Requests(modeltest.Embeddings) {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatalf("decode embeddings request %s: %v", req.Body, err)
		}
		texts = append(texts, body.Input...)
	}
	return texts
}

// legalSample returns the synthetic Vietnamese law the chunker tests use.
func legalSample(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../chunker/testdata/luat_mau.md")
	if err != nil {
		t.Fatalf("read the legal sample: %v", err)
	}
	return string(b)
}

// articleLine matches an article's opening line in the legal sample.
var articleLine = regexp.MustCompile(`(?m)^Điều \d+[a-z]?\.`)

// assertAtMostOneArticlePerChunk fails when a stored Chunk holds more than
// one article line, which a tier that merged sections would do.
func assertAtMostOneArticlePerChunk(t *testing.T, chunks []storedChunk) {
	t.Helper()
	for i, c := range chunks {
		if n := len(articleLine.FindAllStringIndex(c.content, -1)); n > 1 {
			t.Errorf("Chunk %d holds %d article lines, want at most one: %q", i, n, c.content)
		}
	}
}

func TestIngestChunksAVietnameseLawAtItsArticles(t *testing.T) {
	tr := startWith(t, "TRELIS_CHUNKING_STRATEGY=auto")
	alice := tr.principalKey(t, "alice")
	sample := legalSample(t)
	id := tr.upload(t, alice, "luat_mau.md", sample)

	doc := tr.waitForIngest(t, alice, id)
	if doc.ParseStatus != "completed" {
		t.Fatalf("parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}

	chunks := tr.chunks(t, id)
	if len(chunks) == 0 {
		t.Fatal("no Chunks stored for the legal sample")
	}
	firstArticle := utf8.RuneCountInString(sample[:strings.Index(sample, "Điều 1.")])
	articles := 0
	for i, c := range chunks {
		if strings.HasPrefix(c.content, "Điều ") {
			articles++
			continue
		}
		// A Chunk that continues an article carries its breadcrumb; only the
		// preamble before Điều 1 is a section with neither.
		if c.header == "" && c.start >= firstArticle {
			t.Errorf("Chunk %d at rune %d starts inside an article with no breadcrumb: %q", i, c.start, c.content)
		}
	}
	if articles < 2 {
		t.Errorf("%d Chunks start at an article, want the article-aligned split", articles)
	}
	assertAtMostOneArticlePerChunk(t, chunks)
}

func TestIngestStoresParentChunksWithoutEmbeddingThem(t *testing.T) {
	tr := startWith(t,
		"TRELIS_CHUNKING_STRATEGY=auto",
		"TRELIS_CHUNKING_PARENT_CHILD=true",
	)
	alice := tr.principalKey(t, "alice")
	id := tr.upload(t, alice, "luat_mau.md", legalSample(t))

	doc := tr.waitForIngest(t, alice, id)
	if doc.ParseStatus != "completed" {
		t.Fatalf("parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}

	chunks := tr.chunks(t, id)
	if len(chunks) == 0 {
		t.Fatal("no Chunks stored for the legal sample")
	}
	if parents := tr.parentChunkCount(t, id); parents == 0 {
		t.Fatal("no Parent Chunks stored with parent-child mode on")
	}
	for i, c := range chunks {
		if c.parentID == nil {
			t.Errorf("Chunk %d has no parent_id, want it linked to a Parent Chunk", i)
		}
	}

	// The embedder saw each Chunk's text once and no Parent Chunk text: a
	// Parent Chunk is wider than its Chunks, so it is not in this set.
	chunkTexts := make(map[string]bool, len(chunks))
	for _, c := range chunks {
		text := c.content
		if c.header != "" {
			text = c.header + "\n\n" + c.content
		}
		chunkTexts[text] = true
	}
	texts := tr.embeddedTexts(t)
	if len(texts) != len(chunks) {
		t.Fatalf("embedder received %d texts for %d Chunks, want one per Chunk", len(texts), len(chunks))
	}
	for i, text := range texts {
		if !chunkTexts[text] {
			t.Errorf("embedding input %d is not a stored Chunk's text: %q", i, text)
		}
	}
}
