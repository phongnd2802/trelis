package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/chunker"
	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/db/sqlc"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// ingestDocument creates one pending Document owned by ownerID.
func ingestDocument(t *testing.T, store db.Store, ownerID uuid.UUID) *types.Document {
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
	if err := repository.NewDocumentRepository(store).CreateDocument(context.Background(), doc, types.AccessGrants{}); err != nil {
		t.Fatalf("create document: %v", err)
	}
	return doc
}

// fakeIngestStore runs a service's transaction inline; the unit tests use
// fakes in place of Postgres.
type fakeIngestStore struct{}

func (fakeIngestStore) Querier(context.Context) sqlc.Querier { return nil }

func (fakeIngestStore) ExecTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

// fakeIngestDocuments answers with one Document and records the parse
// statuses the service sets.
type fakeIngestDocuments struct {
	interfaces.DocumentRepository
	doc      *types.Document
	statuses []string
}

func (f *fakeIngestDocuments) GetDocumentByID(context.Context, uuid.UUID) (*types.Document, error) {
	return f.doc, nil
}

func (f *fakeIngestDocuments) SetParseStatus(_ context.Context, _ uuid.UUID, status, _ string) error {
	f.statuses = append(f.statuses, status)
	return nil
}

// fakeSourceFile serves one Source File from memory.
type fakeSourceFile struct {
	interfaces.FileService
	content []byte
}

func (f *fakeSourceFile) GetFile(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.content)), nil
}

// fakeDocumentParser returns a fixed Markdown body.
type fakeDocumentParser struct {
	markdown string
}

func (f *fakeDocumentParser) Parse(context.Context, *types.ParseRequest) (*types.ParseResult, error) {
	return &types.ParseResult{MarkdownContent: f.markdown}, nil
}

// fakeIngestChunks records the Chunk sets the service replaces.
type fakeIngestChunks struct {
	interfaces.ChunkRepository
	sets []types.ChunkSet
}

func (f *fakeIngestChunks) ReplaceChunks(_ context.Context, _ uuid.UUID, set types.ChunkSet) error {
	f.sets = append(f.sets, set)
	return nil
}

// readLegalSample returns the synthetic Vietnamese law the chunker tests use.
func readLegalSample(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "chunker", "testdata", "luat_mau.md"))
	if err != nil {
		t.Fatalf("read the legal sample: %v", err)
	}
	return string(b)
}

// runParseAndChunk runs ParseAndChunk over markdown with splitter and returns
// its result and the one Chunk set it stored.
func runParseAndChunk(t *testing.T, markdown string, splitter chunker.SplitterConfig) (types.ChunkingResult, types.ChunkSet) {
	t.Helper()
	doc := &types.Document{
		ID:        uuid.New(),
		Title:     "luat-mau",
		FileName:  "luat_mau.md",
		FileType:  "md",
		ObjectKey: "documents/" + uuid.NewString() + "/source",
	}
	documents := &fakeIngestDocuments{doc: doc}
	chunks := &fakeIngestChunks{}
	svc := NewIngestService(fakeIngestStore{}, documents, chunks,
		&fakeSourceFile{content: []byte("the file's bytes are not the Markdown body here")},
		&fakeDocumentParser{markdown: markdown}, nil, splitter)

	result, err := svc.ParseAndChunk(context.Background(), doc.ID)
	if err != nil {
		t.Fatalf("ParseAndChunk: %v", err)
	}
	if len(documents.statuses) != 1 || documents.statuses[0] != types.ParseStatusProcessing {
		t.Errorf("parse statuses = %q, want processing once", documents.statuses)
	}
	if len(chunks.sets) != 1 {
		t.Fatalf("ReplaceChunks ran %d times, want once", len(chunks.sets))
	}
	return result, chunks.sets[0]
}

// chunkShapes renders the fields that decide a Chunk's content and position,
// so Chunks from two runs can be compared without their random IDs.
func chunkShapes(chunks []*types.Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = fmt.Sprintf("%d|%d|%d|%q|%q", c.Seq, c.StartOffset, c.EndOffset, c.ContextHeader, c.Content)
	}
	return out
}

// articleLine matches an article's opening line, such as "Điều 2a. Đối
// tượng nộp thuế".
var articleLine = regexp.MustCompile(`(?m)^Điều \d+[a-z]?\.`)

// assertAtMostOneArticlePerChunk fails when a Chunk holds more than one
// article line, which a tier that merged sections would do.
func assertAtMostOneArticlePerChunk(t *testing.T, chunks []*types.Chunk) {
	t.Helper()
	for i, c := range chunks {
		if n := len(articleLine.FindAllStringIndex(c.Content, -1)); n > 1 {
			t.Errorf("Chunk %d holds %d article lines, want at most one: %q", i, n, c.Content)
		}
	}
}

func TestParseAndChunkSplitsWithTheConfiguredStrategy(t *testing.T) {
	sample := readLegalSample(t)
	auto := chunker.SplitterConfig{
		ChunkSize: chunker.DefaultChunkSize, ChunkOverlap: chunker.DefaultChunkOverlap, Strategy: chunker.StrategyAuto,
	}
	recursive := auto
	recursive.Strategy = chunker.StrategyRecursive

	autoResult, autoSet := runParseAndChunk(t, sample, auto)
	recursiveResult, recursiveSet := runParseAndChunk(t, sample, recursive)

	if autoResult.Tier != string(chunker.TierHeuristic) {
		t.Errorf("auto tier = %q, want the heuristic tier for the legal sample", autoResult.Tier)
	}
	if recursiveResult.Tier != string(chunker.TierRecursive) {
		t.Errorf("recursive tier = %q, want recursive", recursiveResult.Tier)
	}
	if !slices.Contains(autoResult.TierChain, string(chunker.TierHeuristic)) {
		t.Errorf("auto tier chain = %v, want the heuristic tier in it", autoResult.TierChain)
	}
	if slices.Equal(chunkShapes(autoSet.Chunks), chunkShapes(recursiveSet.Chunks)) {
		t.Error("auto and recursive stored the same Chunks; changing the strategy must change the ingest output")
	}
	if len(autoSet.Parents) != 0 || autoResult.ParentChunks != 0 {
		t.Errorf("parent-child off stored %d Parent Chunks (%d reported), want none",
			len(autoSet.Parents), autoResult.ParentChunks)
	}
	if autoResult.Chunks != len(autoSet.Chunks) || autoResult.Chunks == 0 {
		t.Errorf("result reports %d Chunks for %d stored, want the stored count", autoResult.Chunks, len(autoSet.Chunks))
	}

	// The heuristic tier splits at articles: a Chunk starts at one, or
	// continues one and carries its breadcrumb. The preamble before the first
	// article is the one section with neither.
	firstArticle := utf8.RuneCountInString(sample[:strings.Index(sample, "Điều 1.")])
	articleChunks := 0
	for i, c := range autoSet.Chunks {
		if strings.HasPrefix(c.Content, "Điều ") {
			articleChunks++
			continue
		}
		if c.ContextHeader == "" && c.StartOffset >= firstArticle {
			t.Errorf("auto Chunk %d at %d starts inside an article with no breadcrumb: %q", i, c.StartOffset, c.Content)
		}
	}
	if articleChunks < 2 {
		t.Errorf("%d Chunks start at an article, want the article-aligned split", articleChunks)
	}
	assertAtMostOneArticlePerChunk(t, autoSet.Chunks)
}

func TestParseAndChunkStoresParentChunksOnlyWhenTheModeIsOn(t *testing.T) {
	sample := readLegalSample(t)
	withParents := chunker.SplitterConfig{
		ChunkSize: 300, ChunkOverlap: 60, Strategy: chunker.StrategyAuto, ParentChunkSize: 1500,
	}
	result, set := runParseAndChunk(t, sample, withParents)

	if len(set.Parents) == 0 || result.ParentChunks != len(set.Parents) {
		t.Fatalf("stored %d Parent Chunks (%d reported), want Parent Chunks", len(set.Parents), result.ParentChunks)
	}
	if len(set.ParentOf) != len(set.Chunks) {
		t.Fatalf("%d Parent Chunk links for %d Chunks, want one per Chunk", len(set.ParentOf), len(set.Chunks))
	}
	if result.Chunks != len(set.Chunks) {
		t.Errorf("result reports %d Chunks for %d stored", result.Chunks, len(set.Chunks))
	}
	for i, parent := range set.ParentOf {
		if parent < 0 || parent >= len(set.Parents) {
			t.Fatalf("Chunk %d links to Parent Chunk %d of %d", i, parent, len(set.Parents))
		}
		c, p := set.Chunks[i], set.Parents[parent]
		if c.StartOffset < p.StartOffset || c.EndOffset > p.EndOffset {
			t.Errorf("Chunk %d [%d,%d) lies outside Parent Chunk %d [%d,%d)",
				i, c.StartOffset, c.EndOffset, parent, p.StartOffset, p.EndOffset)
		}
	}

	// The stored Parent Chunks are exactly the chunker's, in Seq order.
	want := chunker.SplitParentChild(sample, withParents)
	if len(want.Parents) != len(set.Parents) {
		t.Fatalf("stored %d Parent Chunks, the chunker split %d", len(set.Parents), len(want.Parents))
	}
	for i, p := range want.Parents {
		got := set.Parents[i]
		if got.Seq != p.Seq || got.Content != p.Content || got.ContextHeader != p.ContextHeader ||
			got.StartOffset != p.Start || got.EndOffset != p.End {
			t.Errorf("Parent Chunk %d = %+v, want the chunker's %+v", i, got, p)
		}
	}

	// Switching the mode off stores the same Chunks and no Parent Chunks.
	withoutParents := withParents
	withoutParents.ParentChunkSize = 0
	offResult, offSet := runParseAndChunk(t, sample, withoutParents)
	if len(offSet.Parents) != 0 || offSet.ParentOf != nil || offResult.ParentChunks != 0 {
		t.Errorf("parent-child off stored %d Parent Chunks and %d links, want none",
			len(offSet.Parents), len(offSet.ParentOf))
	}
	if !slices.Equal(chunkShapes(offSet.Chunks), chunkShapes(set.Chunks)) {
		t.Error("turning parent-child off changed the embedded Chunks, want the same Chunk split")
	}
}

func TestCompleteRefusesWhileAChunkHasNoEmbedding(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	owner, err := store.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := ingestDocument(t, store, owner.ID)
	documents := repository.NewDocumentRepository(store)
	chunks := repository.NewChunkRepository(store)
	if err := store.ExecTx(ctx, func(ctx context.Context) error {
		return chunks.ReplaceChunks(ctx, doc.ID, types.ChunkSet{Chunks: []*types.Chunk{
			{Seq: 0, Content: "Điều 1.", StartOffset: 0, EndOffset: 7},
			{Seq: 1, Content: "Điều 2.", StartOffset: 8, EndOffset: 15},
		}})
	}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}
	unembedded, err := chunks.ListUnembeddedChunks(ctx, doc.ID, 0, 2)
	if err != nil || len(unembedded) != 2 {
		t.Fatalf("ListUnembeddedChunks = %+v, %v; want both Chunks", unembedded, err)
	}
	if err := chunks.SetChunkEmbedding(ctx, unembedded[0].ID, []float32{1, 2}, "test-embedding"); err != nil {
		t.Fatalf("SetChunkEmbedding: %v", err)
	}

	// Complete needs only the repositories; the parser, file service and
	// embedder play no part in it.
	ingest := NewIngestService(store, documents, chunks, nil, nil, nil, chunker.DefaultConfig())
	err = ingest.Complete(ctx, doc.ID)
	if err == nil || !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "without an embedding") {
		t.Fatalf("Complete with one unembedded Chunk = %v, want an error naming the missing count", err)
	}
	got, err := documents.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParseStatus == types.ParseStatusCompleted {
		t.Error("Document is completed with a Chunk that has no embedding")
	}

	if err := chunks.SetChunkEmbedding(ctx, unembedded[1].ID, []float32{3, 4}, "test-embedding"); err != nil {
		t.Fatalf("SetChunkEmbedding: %v", err)
	}
	if err := ingest.Complete(ctx, doc.ID); err != nil {
		t.Fatalf("Complete after every Chunk was embedded: %v", err)
	}
	got, err = documents.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParseStatus != types.ParseStatusCompleted {
		t.Errorf("parse_status = %q, want completed", got.ParseStatus)
	}

	var missing *types.DocumentNotFoundError
	if err := ingest.Complete(ctx, uuid.New()); !errors.As(err, &missing) {
		t.Errorf("Complete(unknown Document) = %v, want *types.DocumentNotFoundError", err)
	}
	// There is no row left to mark when the Document is gone, which is not an
	// error for the workflow that is already ending.
	if err := ingest.MarkFailed(ctx, uuid.New(), "the Source File could not be parsed"); err != nil {
		t.Errorf("MarkFailed(unknown Document) = %v, want nil", err)
	}
}
