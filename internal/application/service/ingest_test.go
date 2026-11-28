package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

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

// fakeIngestEvents records the Ingest Events the service writes and numbers
// them the way the Document's counter does.
type fakeIngestEvents struct {
	interfaces.IngestEventRepository
	inserted []*types.IngestEvent
}

func (f *fakeIngestEvents) InsertIngestEvent(_ context.Context, event *types.IngestEvent) (*types.IngestEvent, error) {
	event.Seq = int64(len(f.inserted) + 1)
	event.CreatedAt = time.Unix(int64(len(f.inserted)), 0)
	f.inserted = append(f.inserted, event)
	return event, nil
}

// missingIngestEvents reports ErrDocumentNotFound, as the repository does when
// the Document row is gone before the event insert.
type missingIngestEvents struct {
	interfaces.IngestEventRepository
}

func (missingIngestEvents) InsertIngestEvent(context.Context, *types.IngestEvent) (*types.IngestEvent, error) {
	return nil, repository.ErrDocumentNotFound
}

// compactEventDetail renders an event's JSON detail without the spacing jsonb
// adds, so a test can compare it with the object the service wrote.
func compactEventDetail(t *testing.T, raw []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("event detail %s is not JSON: %v", raw, err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal event detail %s: %v", raw, err)
	}
	return string(out)
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

// runParseAndChunk runs ParseAndChunk over markdown with splitter and attempt,
// and returns its result, the one Chunk set it stored and the events it wrote.
func runParseAndChunk(t *testing.T, markdown string, splitter chunker.SplitterConfig, attempt int) (types.ChunkingResult, types.ChunkSet, *fakeIngestEvents) {
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
	events := &fakeIngestEvents{}
	svc := NewIngestService(fakeIngestStore{}, documents, chunks, events,
		&fakeSourceFile{content: []byte("the file's bytes are not the Markdown body here")},
		&fakeDocumentParser{markdown: markdown}, nil, splitter)

	result, err := svc.ParseAndChunk(context.Background(), doc.ID, attempt)
	if err != nil {
		t.Fatalf("ParseAndChunk: %v", err)
	}
	if len(documents.statuses) != 1 || documents.statuses[0] != types.ParseStatusProcessing {
		t.Errorf("parse statuses = %q, want processing once", documents.statuses)
	}
	if len(chunks.sets) != 1 {
		t.Fatalf("ReplaceChunks ran %d times, want once", len(chunks.sets))
	}
	return result, chunks.sets[0], events
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

	autoResult, autoSet, _ := runParseAndChunk(t, sample, auto, 1)
	recursiveResult, recursiveSet, _ := runParseAndChunk(t, sample, recursive, 1)

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

func TestParseAndChunkWritesAStartedAndAFinishedEvent(t *testing.T) {
	sample := readLegalSample(t)
	config := chunker.SplitterConfig{
		ChunkSize: chunker.DefaultChunkSize, ChunkOverlap: chunker.DefaultChunkOverlap, Strategy: chunker.StrategyAuto,
	}
	result, set, events := runParseAndChunk(t, sample, config, 3)

	if len(events.inserted) != 2 {
		t.Fatalf("ParseAndChunk wrote %d events, want parse_started and parse_finished", len(events.inserted))
	}
	started, finished := events.inserted[0], events.inserted[1]
	if started.Type != types.IngestEventParseStarted || started.Seq >= finished.Seq {
		t.Errorf("events = %s at %d, %s at %d; want parse_started before parse_finished",
			started.Type, started.Seq, finished.Type, finished.Seq)
	}
	var startedDetail types.ParseStartedDetail
	if err := json.Unmarshal(started.Detail, &startedDetail); err != nil {
		t.Fatalf("parse_started detail %s: %v", started.Detail, err)
	}
	if startedDetail.Attempt != 3 {
		t.Errorf("parse_started attempt = %d, want the activity's 3", startedDetail.Attempt)
	}
	if finished.Type != types.IngestEventParseFinished {
		t.Errorf("second event = %s, want parse_finished", finished.Type)
	}
	var finishedDetail types.ParseFinishedDetail
	if err := json.Unmarshal(finished.Detail, &finishedDetail); err != nil {
		t.Fatalf("parse_finished detail %s: %v", finished.Detail, err)
	}
	if finishedDetail.Chunks != len(set.Chunks) || finishedDetail.Chunks != result.Chunks {
		t.Errorf("parse_finished chunks = %d, want the %d stored Chunks", finishedDetail.Chunks, len(set.Chunks))
	}
}

func TestParseAndChunkStoresParentChunksOnlyWhenTheModeIsOn(t *testing.T) {
	sample := readLegalSample(t)
	withParents := chunker.SplitterConfig{
		ChunkSize: 300, ChunkOverlap: 60, Strategy: chunker.StrategyAuto, ParentChunkSize: 1500,
	}
	result, set, _ := runParseAndChunk(t, sample, withParents, 1)

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
	offResult, offSet, _ := runParseAndChunk(t, sample, withoutParents, 1)
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
	events := repository.NewIngestEventRepository(store)
	ingest := NewIngestService(store, documents, chunks, events, nil, nil, nil, chunker.DefaultConfig())
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
	written, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 0 {
		t.Errorf("failed Complete wrote %+v, want no completed event", written)
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
	written, err = events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0].Type != types.IngestEventCompleted || compactEventDetail(t, written[0].Detail) != "{}" {
		t.Errorf("completed event = %+v, want one completed with an empty detail", written)
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

// failingChunks fails every ReplaceChunks, so a parse transaction rolls back.
type failingChunks struct {
	interfaces.ChunkRepository
	err error
}

func (f *failingChunks) ReplaceChunks(context.Context, uuid.UUID, types.ChunkSet) error {
	return f.err
}

// gatedChunks wraps a ChunkRepository and holds its first embedding update
// after the row has been updated, so a test can run a Document delete while
// the embed transaction holds a Chunk row.
type gatedChunks struct {
	interfaces.ChunkRepository
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedChunks) SetChunkEmbedding(ctx context.Context, chunkID uuid.UUID, vector []float32, model string) error {
	if err := g.ChunkRepository.SetChunkEmbedding(ctx, chunkID, vector, model); err != nil {
		return err
	}
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return nil
}

// isDeadlock reports PostgreSQL's deadlock_detected (40P01).
func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func TestAFailedChunkWriteLeavesNoParseFinishedEvent(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	owner, err := store.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := ingestDocument(t, store, owner.ID)
	events := repository.NewIngestEventRepository(store)
	ingest := NewIngestService(store, repository.NewDocumentRepository(store),
		&failingChunks{err: errors.New("insert chunk 0: connection reset")}, events,
		&fakeSourceFile{content: []byte("bytes")},
		&fakeDocumentParser{markdown: "# Notes\n\nText.\n"}, nil, chunker.DefaultConfig())

	// Both attempts fail on the Chunk write. parse_started is written on each
	// one; parse_finished is inside the rolled-back transaction and never
	// appears.
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := ingest.ParseAndChunk(ctx, doc.ID, attempt); err == nil {
			t.Fatalf("attempt %d: ParseAndChunk = nil, want the Chunk insert error", attempt)
		}
	}
	written, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 {
		t.Fatalf("events after two failed attempts = %+v, want two parse_started", written)
	}
	for i, event := range written {
		if event.Type != types.IngestEventParseStarted {
			t.Errorf("event %d = %s, want parse_started only", i, event.Type)
		}
		var detail types.ParseStartedDetail
		if err := json.Unmarshal(event.Detail, &detail); err != nil || detail.Attempt != i+1 {
			t.Errorf("event %d attempt = %d (%v), want %d", i, detail.Attempt, err, i+1)
		}
	}
}

func TestParseAndChunkMapsAMissingEventDocumentToNotFound(t *testing.T) {
	doc := &types.Document{
		ID:        uuid.New(),
		Title:     "luat-thue.txt",
		FileName:  "luat-thue.txt",
		FileType:  "txt",
		ObjectKey: "documents/" + uuid.NewString() + "/source",
	}
	documents := &fakeIngestDocuments{doc: doc}
	ingest := NewIngestService(fakeIngestStore{}, documents, &fakeIngestChunks{}, missingIngestEvents{},
		&fakeSourceFile{content: []byte("bytes")},
		&fakeDocumentParser{markdown: "# Notes\n\nText.\n"}, nil, chunker.DefaultConfig())

	_, err := ingest.ParseAndChunk(context.Background(), doc.ID, 1)
	var missing *types.DocumentNotFoundError
	if !errors.As(err, &missing) {
		t.Fatalf("ParseAndChunk = %v, want *types.DocumentNotFoundError when the Document row is gone", err)
	}
	if len(documents.statuses) != 0 {
		t.Errorf("statuses = %q, want none after the event insert found no Document", documents.statuses)
	}
}

func TestEmbedBatchWritesOneFinishedEventPerStoredBatch(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	owner, err := store.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := ingestDocument(t, store, owner.ID)
	chunks := repository.NewChunkRepository(store)
	if err := store.ExecTx(ctx, func(ctx context.Context) error {
		return chunks.ReplaceChunks(ctx, doc.ID, types.ChunkSet{Chunks: []*types.Chunk{
			{Seq: 0, Content: "Điều 1.", StartOffset: 0, EndOffset: 7},
			{Seq: 1, Content: "Điều 2.", StartOffset: 8, EndOffset: 15},
		}})
	}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}
	events := repository.NewIngestEventRepository(store)
	embedder := &fakeEmbedder{vectors: [][]float32{{1, 2}, {3, 4}}, model: "test-embedding"}
	ingest := NewIngestService(store, repository.NewDocumentRepository(store), chunks, events,
		nil, nil, embedder, chunker.DefaultConfig())

	embedded, err := ingest.EmbedBatch(ctx, doc.ID, 0, 2)
	if err != nil || embedded != 2 {
		t.Fatalf("EmbedBatch = %d, %v; want both Chunks embedded", embedded, err)
	}
	written, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0].Type != types.IngestEventEmbedBatchFinished {
		t.Fatalf("events = %+v, want one embed_batch_finished", written)
	}
	var detail types.EmbedBatchFinishedDetail
	if err := json.Unmarshal(written[0].Detail, &detail); err != nil {
		t.Fatalf("embed_batch_finished detail %s: %v", written[0].Detail, err)
	}
	if detail != (types.EmbedBatchFinishedDetail{From: 0, To: 2, Chunks: 2}) {
		t.Errorf("detail = %+v, want from 0 to 2 with 2 Chunks", detail)
	}

	// The range is already embedded, so this attempt has nothing to store and
	// must not write a second finished event.
	embedded, err = ingest.EmbedBatch(ctx, doc.ID, 0, 2)
	if err != nil || embedded != 0 {
		t.Fatalf("EmbedBatch of an embedded range = %d, %v; want a no-op", embedded, err)
	}
	written, err = events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 {
		t.Errorf("events after the no-op = %+v, want no embed_batch_finished for an empty batch", written)
	}
}

func TestEmbedBatchAndDocumentDeleteDoNotDeadlock(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	store := db.NewStore(pool)
	owner, err := store.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := ingestDocument(t, store, owner.ID)
	chunks := repository.NewChunkRepository(store)
	if err := store.ExecTx(ctx, func(ctx context.Context) error {
		return chunks.ReplaceChunks(ctx, doc.ID, types.ChunkSet{Chunks: []*types.Chunk{
			{Seq: 0, Content: "Điều 1.", StartOffset: 0, EndOffset: 7},
			{Seq: 1, Content: "Điều 2.", StartOffset: 8, EndOffset: 15},
		}})
	}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}

	gated := &gatedChunks{ChunkRepository: chunks, entered: make(chan struct{}), release: make(chan struct{})}
	embedder := &fakeEmbedder{vectors: [][]float32{{1, 2}, {3, 4}}, model: "test-embedding"}
	ingest := NewIngestService(store, repository.NewDocumentRepository(store), gated,
		repository.NewIngestEventRepository(store), nil, nil, embedder, chunker.DefaultConfig())

	batchErr := make(chan error, 1)
	go func() {
		_, err := ingest.EmbedBatch(ctx, doc.ID, 0, 2)
		batchErr <- err
	}()
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("EmbedBatch did not reach its first Chunk update")
	}

	deleteErr := make(chan error, 1)
	go func() { deleteErr <- repository.NewDocumentRepository(store).DeleteDocument(ctx, doc.ID) }()
	// Let the delete take the Document row and start its cascade before the
	// embed transaction asks for anything else. With the old order (Chunks
	// first, event last) the two then wait on each other; with the event
	// first, the delete waits on the Document row the embed transaction holds.
	time.Sleep(200 * time.Millisecond)
	close(gated.release)

	batch, del := <-batchErr, <-deleteErr
	if isDeadlock(batch) || isDeadlock(del) {
		t.Fatalf("EmbedBatch and DeleteDocument deadlocked: EmbedBatch = %v, DeleteDocument = %v", batch, del)
	}
	if del != nil {
		t.Fatalf("DeleteDocument = %v, want the delete to finish after the batch", del)
	}
	var missing *types.DocumentNotFoundError
	if batch != nil && !errors.Is(batch, repository.ErrDocumentNotFound) && !errors.As(batch, &missing) {
		t.Fatalf("EmbedBatch = %v, want nil or a not-found error", batch)
	}
	if _, err := repository.NewDocumentRepository(store).GetDocumentByID(ctx, doc.ID); !errors.Is(err, repository.ErrDocumentNotFound) {
		t.Errorf("GetDocumentByID after the delete = %v, want ErrDocumentNotFound", err)
	}
	for _, table := range []string{"chunks", "ingest_events"} {
		var rows int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE document_id = $1", doc.ID).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if rows != 0 {
			t.Errorf("%s holds %d rows for the deleted Document, want 0 by cascade", table, rows)
		}
	}
}

func TestMarkFailedWritesTheFailedEventWithItsSafeReason(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	owner, err := store.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := ingestDocument(t, store, owner.ID)
	events := repository.NewIngestEventRepository(store)
	documents := repository.NewDocumentRepository(store)
	ingest := NewIngestService(store, documents, repository.NewChunkRepository(store), events,
		nil, nil, nil, chunker.DefaultConfig())

	const reason = "the Source File could not be parsed"
	if err := ingest.MarkFailed(ctx, doc.ID, reason); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	written, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0].Type != types.IngestEventFailed {
		t.Fatalf("events = %+v, want one failed", written)
	}
	var detail types.FailedDetail
	if err := json.Unmarshal(written[0].Detail, &detail); err != nil {
		t.Fatalf("failed detail %s: %v", written[0].Detail, err)
	}
	if detail.Reason != reason {
		t.Errorf("failed reason = %q, want the stored safe reason %q", detail.Reason, reason)
	}
	stored, err := documents.GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ParseStatus != types.ParseStatusFailed || stored.ParseError != reason {
		t.Errorf("Document = %q (%q), want failed with the same reason", stored.ParseStatus, stored.ParseError)
	}

	// A Document deleted before the failure has no row to mark and gets no
	// event either.
	if err := ingest.MarkFailed(ctx, uuid.New(), reason); err != nil {
		t.Errorf("MarkFailed(unknown Document) = %v, want nil", err)
	}
}
