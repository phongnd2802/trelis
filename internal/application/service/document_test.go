package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/application/service/file"
	"github.com/phongnd2802/trelis/internal/application/service/file/filetest"
	"github.com/phongnd2802/trelis/internal/chunker"
	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	werrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// uploadRef is one Document upload a workflow call named.
type uploadRef struct {
	DocumentID, UploadID uuid.UUID
}

// fakeIngestControl records the workflow starts, terminations and cleanups
// the Document service asks for, and can fail any of them.
type fakeIngestControl struct {
	starts       []uploadRef
	terminates   []uploadRef
	deletes      []uploadRef
	startErr     error
	terminateErr error
	deleteErr    error
}

func (f *fakeIngestControl) StartIngest(_ context.Context, documentID, uploadID uuid.UUID) error {
	f.starts = append(f.starts, uploadRef{DocumentID: documentID, UploadID: uploadID})
	return f.startErr
}

func (f *fakeIngestControl) TerminateIngest(_ context.Context, documentID, uploadID uuid.UUID) error {
	f.terminates = append(f.terminates, uploadRef{DocumentID: documentID, UploadID: uploadID})
	return f.terminateErr
}

func (f *fakeIngestControl) StartDocumentDelete(_ context.Context, documentID, uploadID uuid.UUID) error {
	f.deletes = append(f.deletes, uploadRef{DocumentID: documentID, UploadID: uploadID})
	return f.deleteErr
}

// documentServiceFor returns the Document service on store, files and starter.
func documentServiceFor(store db.Store, files interfaces.FileService, starter *fakeIngestControl) *documentService {
	return &documentService{
		store:      store,
		repo:       repository.NewDocumentRepository(store),
		principals: repository.NewPrincipalRepository(store),
		events:     repository.NewIngestEventRepository(store),
		chunks:     repository.NewChunkRepository(store),
		fileSvc:    files,
		ingest:     starter,
		cleanup:    starter,
	}
}

// principalContext creates principalName and returns a context authenticated
// as it.
func principalContext(t *testing.T, store db.Store, principalName string) (context.Context, types.Principal) {
	t.Helper()
	p, err := store.Querier(context.Background()).CreatePrincipal(context.Background(), principalName)
	if err != nil {
		t.Fatalf("create principal %s: %v", principalName, err)
	}
	return types.WithPrincipal(context.Background(), types.Principal{ID: p.ID, Name: p.Name}), types.Principal{ID: p.ID, Name: p.Name}
}

// uploadHeader builds the multipart file header a handler would pass to the
// service.
func uploadHeader(t *testing.T, name, content string) *multipart.FileHeader {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if err := req.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("parse the upload: %v", err)
	}
	t.Cleanup(func() { _ = req.MultipartForm.RemoveAll() })
	return req.MultipartForm.File["file"][0]
}

// seedChunks stores one Chunk and its Parent Chunk for doc.
func seedChunks(t *testing.T, store db.Store, doc *types.Document) {
	t.Helper()
	chunks := repository.NewChunkRepository(store)
	if err := store.ExecTx(context.Background(), func(ctx context.Context) error {
		return chunks.ReplaceChunks(ctx, doc.ID, types.ChunkSet{
			Parents:  []*types.ParentChunk{{Seq: 0, Content: "old version", StartOffset: 0, EndOffset: 11}},
			Chunks:   []*types.Chunk{{Seq: 0, Content: "old version", StartOffset: 0, EndOffset: 11}},
			ParentOf: []int{0},
		})
	}); err != nil {
		t.Fatalf("seed the old Chunks: %v", err)
	}
}

// moveDocumentToUpload changes the Document's current upload without going
// through a file replacement, so a stale activity can be tested against a
// newer upload.
func moveDocumentToUpload(t *testing.T, store db.Store, doc *types.Document, uploadID uuid.UUID) {
	t.Helper()
	updated := *doc
	updated.UploadID = uploadID
	if _, err := repository.NewDocumentRepository(store).ReplaceDocumentUpload(context.Background(), &updated, doc.UploadID); err != nil {
		t.Fatalf("move Document to upload %s: %v", uploadID, err)
	}
	doc.UploadID = uploadID
}

func TestReplaceDocumentFileReplacesTheRowChunksAndUpload(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	store := db.NewStore(pool)
	ownerCtx, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	seedChunks(t, store, doc)

	files := filetest.NewMemory()
	oldKey := file.SourceKey(doc.ID, doc.UploadID)
	files.PutObject(oldKey, []byte("old version"), time.Now())
	starter := &fakeIngestControl{}
	svc := documentServiceFor(store, files, starter)

	content := "# New version\n\nDifferent text.\n"
	got, err := svc.ReplaceDocumentFile(ownerCtx, doc.ID, uploadHeader(t, "new.md", content))
	if err != nil {
		t.Fatalf("ReplaceDocumentFile: %v", err)
	}
	if got.ID != doc.ID || got.FileName != "new.md" || got.Title != "new.md" || got.FileType != "md" ||
		got.FileSize != int64(len(content)) || got.ParseStatus != types.ParseStatusPending ||
		got.UploadID == doc.UploadID {
		t.Errorf("replaced Document = %+v, want the new file metadata, pending status and a new upload", got)
	}
	if got.FileHash == doc.FileHash {
		t.Error("file hash did not change with the content")
	}

	stored, err := repository.NewDocumentRepository(store).GetDocumentByID(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UploadID != got.UploadID || stored.FileName != "new.md" || stored.ObjectKey != file.SourceKey(doc.ID, got.UploadID) ||
		stored.ParseStatus != types.ParseStatusPending {
		t.Errorf("stored Document = %+v, want the new upload and file metadata", stored)
	}

	unembedded, err := repository.NewChunkRepository(store).ListUnembeddedChunks(ctx, doc.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unembedded) != 0 {
		t.Errorf("stored Chunks = %+v, want the old version gone", unembedded)
	}
	var parents int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM parent_chunks WHERE document_id = $1", doc.ID).Scan(&parents); err != nil {
		t.Fatalf("count Parent Chunks: %v", err)
	}
	if parents != 0 {
		t.Errorf("Parent Chunks = %d, want the old version gone", parents)
	}

	events, err := repository.NewIngestEventRepository(store).ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != types.IngestEventUploadReplaced ||
		events[0].UploadID != got.UploadID {
		t.Fatalf("events = %+v, want one upload_replaced for the new upload", events)
	}
	var detail types.UploadReplacedDetail
	if err := json.Unmarshal(events[0].Detail, &detail); err != nil {
		t.Fatalf("upload_replaced detail %s: %v", events[0].Detail, err)
	}
	if detail.UploadID != got.UploadID {
		t.Errorf("upload_replaced detail = %+v, want the new upload %s", detail, got.UploadID)
	}

	newKey := file.SourceKey(doc.ID, got.UploadID)
	if keys := files.Keys(); len(keys) != 2 || !slices.Contains(keys, oldKey) || !slices.Contains(keys, newKey) {
		t.Errorf("store holds %v, want the old object %q kept and the new one %q written", keys, oldKey, newKey)
	}
	wantTerminated := uploadRef{DocumentID: doc.ID, UploadID: doc.UploadID}
	if len(starter.terminates) != 1 || starter.terminates[0] != wantTerminated {
		t.Errorf("TerminateIngest calls = %v, want the previous upload %v", starter.terminates, wantTerminated)
	}
	wantStarted := uploadRef{DocumentID: doc.ID, UploadID: got.UploadID}
	if len(starter.starts) != 1 || starter.starts[0] != wantStarted {
		t.Errorf("StartIngest calls = %v, want the new upload %v", starter.starts, wantStarted)
	}
}

// failingChunkDelete makes the replacement transaction roll back after the
// row update and before the event insert.
type failingChunkDelete struct {
	interfaces.ChunkRepository
	err error
}

func (f *failingChunkDelete) DeleteChunks(context.Context, uuid.UUID) error { return f.err }

func TestReplaceDocumentFileRemovesTheNewObjectAndKeepsTheRowOnFailure(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	ownerCtx, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	seedChunks(t, store, doc)

	files := filetest.NewMemory()
	oldKey := file.SourceKey(doc.ID, doc.UploadID)
	files.PutObject(oldKey, []byte("old version"), time.Now())
	starter := &fakeIngestControl{}
	svc := documentServiceFor(store, files, starter)
	svc.chunks = &failingChunkDelete{err: errors.New("delete chunks failed")}

	_, err := svc.ReplaceDocumentFile(ownerCtx, doc.ID, uploadHeader(t, "new.md", "new version"))
	if err == nil {
		t.Fatal("ReplaceDocumentFile = nil, want the failed Chunk delete")
	}
	stored, getErr := repository.NewDocumentRepository(store).GetDocumentByID(ctx, doc.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.UploadID != doc.UploadID || stored.FileName != doc.FileName || stored.ParseStatus != types.ParseStatusPending {
		t.Errorf("Document = %+v, want the previous upload and file metadata", stored)
	}
	if keys := files.Keys(); len(keys) != 1 || keys[0] != oldKey {
		t.Errorf("store holds %v, want only the previous object after the rollback", keys)
	}
	unembedded, err := repository.NewChunkRepository(store).ListUnembeddedChunks(ctx, doc.ID, 0, 10)
	if err != nil || len(unembedded) != 1 {
		t.Errorf("Chunks after the rollback = %+v, %v; want the old Chunk kept", unembedded, err)
	}
	events, err := repository.NewIngestEventRepository(store).ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil || len(events) != 0 {
		t.Errorf("events after the rollback = %+v, %v; want none", events, err)
	}
	if len(starter.terminates) != 0 || len(starter.starts) != 0 {
		t.Errorf("workflow calls = %v, %v; want none after the rollback", starter.terminates, starter.starts)
	}
}

// savingThenRacing replaces the Document's upload from inside SaveFile, after
// the service has read the Document and before its transaction updates it.
type savingThenRacing struct {
	interfaces.FileService
	onSave func()
	once   sync.Once
}

func (s *savingThenRacing) SaveFile(ctx context.Context, file *multipart.FileHeader, documentID, uploadID uuid.UUID) (string, error) {
	s.once.Do(s.onSave)
	return s.FileService.SaveFile(ctx, file, documentID, uploadID)
}

func TestReplaceDocumentFileAnswers412WhenTheUploadChanged(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	ownerCtx, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	seedChunks(t, store, doc)

	files := filetest.NewMemory()
	oldKey := file.SourceKey(doc.ID, doc.UploadID)
	files.PutObject(oldKey, []byte("old version"), time.Now())
	racingUpload := uuid.New()
	wrapped := &savingThenRacing{FileService: files}
	wrapped.onSave = func() {
		updated := *doc
		updated.UploadID = racingUpload
		if _, err := repository.NewDocumentRepository(store).ReplaceDocumentUpload(context.Background(), &updated, doc.UploadID); err != nil {
			t.Errorf("racing re-upload: %v", err)
		}
	}
	starter := &fakeIngestControl{}
	svc := documentServiceFor(store, wrapped, starter)

	_, err := svc.ReplaceDocumentFile(ownerCtx, doc.ID, uploadHeader(t, "new.md", "new version"))
	appErr, ok := werrors.IsAppError(err)
	if !ok || appErr.Code != werrors.ErrPreconditionFailed || appErr.HTTPCode != 412 {
		t.Fatalf("ReplaceDocumentFile = %v, want a 412 precondition failure", err)
	}
	if appErr.Message != "Document changed while uploading; retry" {
		t.Errorf("precondition message = %q, want the retry sentence", appErr.Message)
	}
	stored, getErr := repository.NewDocumentRepository(store).GetDocumentByID(ctx, doc.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.UploadID != racingUpload {
		t.Errorf("Document upload = %s, want the racing upload %s", stored.UploadID, racingUpload)
	}
	if keys := files.Keys(); len(keys) != 1 || keys[0] != oldKey {
		t.Errorf("store holds %v, want the losing upload's object deleted", keys)
	}
	events, err := repository.NewIngestEventRepository(store).ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil || len(events) != 0 {
		t.Errorf("events = %+v, %v; want no upload_replaced for the losing upload", events, err)
	}
	if len(starter.terminates) != 0 || len(starter.starts) != 0 {
		t.Errorf("workflow calls = %v, %v; want none for the losing upload", starter.terminates, starter.starts)
	}
}

func TestTheReplacedDocumentKeepsItsAccessList(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	ownerCtx, owner := principalContext(t, store, "alice")
	readerCtx, reader := principalContext(t, store, "bob")
	documentID, uploadID := uuid.New(), uuid.New()
	doc := &types.Document{
		ID:        documentID,
		OwnerID:   owner.ID,
		Title:     "old.txt",
		FileName:  "old.txt",
		FileType:  "txt",
		FileSize:  11,
		FileHash:  uuid.NewString(),
		UploadID:  uploadID,
		ObjectKey: file.SourceKey(documentID, uploadID),
	}
	grants := types.AccessGrants{PrincipalIDs: []uuid.UUID{reader.ID}}
	if err := store.ExecTx(ctx, func(ctx context.Context) error {
		return repository.NewDocumentRepository(store).CreateDocument(ctx, doc, grants)
	}); err != nil {
		t.Fatalf("create the Document: %v", err)
	}

	files := filetest.NewMemory()
	svc := documentServiceFor(store, files, &fakeIngestControl{})
	got, err := svc.ReplaceDocumentFile(ownerCtx, doc.ID, uploadHeader(t, "new.txt", "new version"))
	if err != nil {
		t.Fatalf("ReplaceDocumentFile: %v", err)
	}
	if got.AccessList == nil || !slices.Contains(got.AccessList.Principals, "alice") ||
		!slices.Contains(got.AccessList.Principals, "bob") {
		t.Errorf("access list = %+v, want the owner and bob kept", got.AccessList)
	}
	read, err := svc.GetDocumentByID(readerCtx, doc.ID)
	if err != nil || read.ID != doc.ID {
		t.Errorf("reader GET after replace = %+v, %v; want the Document still readable", read, err)
	}

	// A reader who is not the owner cannot replace it, and the handler's
	// pre-check says so without reading a body.
	if err := svc.EnsureCanReplace(ownerCtx, doc.ID); err != nil {
		t.Errorf("EnsureCanReplace(owner) = %v, want nil", err)
	}
	err = svc.EnsureCanReplace(readerCtx, doc.ID)
	appErr, ok := werrors.IsAppError(err)
	if !ok || appErr.Code != werrors.ErrForbidden {
		t.Errorf("reader EnsureCanReplace = %v, want the forbidden error", err)
	}
	_, err = svc.ReplaceDocumentFile(readerCtx, doc.ID, uploadHeader(t, "newer.txt", "newer version"))
	appErr, ok = werrors.IsAppError(err)
	if !ok || appErr.Code != werrors.ErrForbidden {
		t.Errorf("reader ReplaceDocumentFile = %v, want the forbidden error", err)
	}
}

// deletingAfterReupload commits a re-upload between the service's read and
// its delete, so the cleanup must use the upload the deleted row carried.
type deletingAfterReupload struct {
	interfaces.DocumentRepository
	onDelete func()
	once     sync.Once
}

func (d *deletingAfterReupload) DeleteDocument(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	d.once.Do(d.onDelete)
	return d.DocumentRepository.DeleteDocument(ctx, id)
}

func TestDeleteDocumentCancelsTheUploadFromTheDeletedRow(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	ownerCtx, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	oldUpload, newUpload := doc.UploadID, uuid.New()
	starter := &fakeIngestControl{}
	svc := documentServiceFor(store, filetest.NewMemory(), starter)
	racing := &deletingAfterReupload{DocumentRepository: repository.NewDocumentRepository(store)}
	racing.onDelete = func() {
		updated := *doc
		updated.UploadID = newUpload
		if _, err := repository.NewDocumentRepository(store).ReplaceDocumentUpload(ctx, &updated, oldUpload); err != nil {
			t.Errorf("racing re-upload: %v", err)
		}
		doc.UploadID = newUpload
	}
	svc.repo = racing

	if err := svc.DeleteDocument(ownerCtx, doc.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	want := uploadRef{DocumentID: doc.ID, UploadID: newUpload}
	if len(starter.deletes) != 1 || starter.deletes[0] != want {
		t.Errorf("StartDocumentDelete calls = %v, want the deleted row's upload %v", starter.deletes, want)
	}
	if _, err := svc.GetDocumentByID(ownerCtx, doc.ID); err == nil {
		t.Error("the Document is still readable after the delete")
	}
}

func TestParseAndChunkWithAStaleUploadWritesNothing(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	_, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	staleUpload := doc.UploadID
	moveDocumentToUpload(t, store, doc, uuid.New())

	events := repository.NewIngestEventRepository(store)
	svc := NewIngestService(store, repository.NewDocumentRepository(store),
		repository.NewChunkRepository(store), events, &fakeSourceFile{content: []byte("bytes")},
		&fakeDocumentParser{markdown: "# Notes\n\nText.\n"}, nil, chunker.DefaultConfig())

	_, err := svc.ParseAndChunk(ctx, doc.ID, staleUpload, 1)
	var superseded *types.SupersededUploadError
	if !errors.As(err, &superseded) {
		t.Fatalf("ParseAndChunk = %v, want *types.SupersededUploadError", err)
	}
	unembedded, err := repository.NewChunkRepository(store).ListUnembeddedChunks(ctx, doc.ID, 0, 10)
	if err != nil || len(unembedded) != 0 {
		t.Errorf("Chunks = %+v, %v; want none for the stale upload", unembedded, err)
	}
	written, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil || len(written) != 0 {
		t.Errorf("events = %+v, %v; want none for the stale upload", written, err)
	}
}

// gatedParser holds its first Parse until the test releases it, so the
// Document's upload can be replaced between the read and the commit.
type gatedParser struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *gatedParser) Parse(context.Context, *types.ParseRequest) (*types.ParseResult, error) {
	p.once.Do(func() {
		close(p.entered)
		<-p.release
	})
	return &types.ParseResult{MarkdownContent: "# Notes\n\nText.\n"}, nil
}

func TestParseAndChunkChecksTheUploadUnderTheDocumentRowLock(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	_, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	events := repository.NewIngestEventRepository(store)
	parser := &gatedParser{entered: make(chan struct{}), release: make(chan struct{})}
	svc := NewIngestService(store, repository.NewDocumentRepository(store),
		repository.NewChunkRepository(store), events, &fakeSourceFile{content: []byte("bytes")},
		parser, nil, chunker.DefaultConfig())

	result := make(chan error, 1)
	go func() {
		_, err := svc.ParseAndChunk(ctx, doc.ID, doc.UploadID, 1)
		result <- err
	}()
	select {
	case <-parser.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("ParseAndChunk did not reach the parser")
	}
	// A re-upload wins while the parse is in flight.
	moveDocumentToUpload(t, store, doc, uuid.New())
	close(parser.release)

	err := <-result
	var superseded *types.SupersededUploadError
	if !errors.As(err, &superseded) {
		t.Fatalf("ParseAndChunk = %v, want *types.SupersededUploadError at commit", err)
	}
	unembedded, err := repository.NewChunkRepository(store).ListUnembeddedChunks(ctx, doc.ID, 0, 10)
	if err != nil || len(unembedded) != 0 {
		t.Errorf("Chunks = %+v, %v; want none committed by the stale parse", unembedded, err)
	}
	written, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range written {
		if event.Type == types.IngestEventParseFinished {
			t.Errorf("events = %+v, want no parse_finished from the stale parse", written)
		}
	}
}

func TestEmbedBatchWithAStaleUploadWritesNothing(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	_, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	seedChunks(t, store, doc)
	staleUpload := doc.UploadID
	moveDocumentToUpload(t, store, doc, uuid.New())

	events := repository.NewIngestEventRepository(store)
	embedder := &fakeEmbedder{vectors: [][]float32{{1, 2}}, model: "test-embedding"}
	svc := NewIngestService(store, repository.NewDocumentRepository(store),
		repository.NewChunkRepository(store), events, nil, nil, embedder, chunker.DefaultConfig())

	_, err := svc.EmbedBatch(ctx, doc.ID, staleUpload, 0, 1)
	var superseded *types.SupersededUploadError
	if !errors.As(err, &superseded) {
		t.Fatalf("EmbedBatch = %v, want *types.SupersededUploadError", err)
	}
	if embedder.calls != 0 {
		t.Errorf("the embedding model was called %d times, want no call for a superseded batch", embedder.calls)
	}
	missing, err := repository.NewChunkRepository(store).CountChunksWithoutEmbedding(ctx, doc.ID)
	if err != nil || missing != 1 {
		t.Errorf("unembedded Chunks = %d, %v; want the stale batch to store no vector", missing, err)
	}
	written, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil || len(written) != 0 {
		t.Errorf("events = %+v, %v; want none for the stale batch", written, err)
	}
}

func TestCompleteWithAStaleUploadDoesNotCompleteTheNewVersion(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	_, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	staleUpload := doc.UploadID
	moveDocumentToUpload(t, store, doc, uuid.New())

	events := repository.NewIngestEventRepository(store)
	svc := NewIngestService(store, repository.NewDocumentRepository(store),
		repository.NewChunkRepository(store), events, nil, nil, nil, chunker.DefaultConfig())

	err := svc.Complete(ctx, doc.ID, staleUpload)
	var superseded *types.SupersededUploadError
	if !errors.As(err, &superseded) {
		t.Fatalf("Complete = %v, want *types.SupersededUploadError", err)
	}
	stored, getErr := repository.NewDocumentRepository(store).GetDocumentByID(ctx, doc.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.ParseStatus == types.ParseStatusCompleted {
		t.Error("the stale complete marked the new version completed")
	}
	written, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil || len(written) != 0 {
		t.Errorf("events = %+v, %v; want none for the stale complete", written, err)
	}
}

func TestMarkFailedWithAStaleUploadDoesNotMarkTheNewVersion(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	_, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	staleUpload := doc.UploadID
	moveDocumentToUpload(t, store, doc, uuid.New())

	events := repository.NewIngestEventRepository(store)
	svc := NewIngestService(store, repository.NewDocumentRepository(store),
		repository.NewChunkRepository(store), events, nil, nil, nil, chunker.DefaultConfig())

	err := svc.MarkFailed(ctx, doc.ID, staleUpload, "the Source File could not be parsed")
	var superseded *types.SupersededUploadError
	if !errors.As(err, &superseded) {
		t.Fatalf("MarkFailed = %v, want *types.SupersededUploadError", err)
	}
	stored, getErr := repository.NewDocumentRepository(store).GetDocumentByID(ctx, doc.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.ParseStatus == types.ParseStatusFailed || stored.ParseError != "" {
		t.Errorf("the stale failure marked the new version failed: %+v", stored)
	}
	written, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil || len(written) != 0 {
		t.Errorf("events = %+v, %v; want none for the stale failure", written, err)
	}
}

func TestDeleteSupersededUploadsRemovesEarlierUploadsButKeepsNewerOnes(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	_, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	currentUpload := doc.UploadID
	staleUpload, newerUpload := uuid.New(), uuid.New()
	files := filetest.NewMemory()
	currentKey := file.SourceKey(doc.ID, currentUpload)
	staleKey := file.SourceKey(doc.ID, staleUpload)
	newerKey := file.SourceKey(doc.ID, newerUpload)
	now := time.Now()
	files.PutObject(staleKey, []byte("stale"), now.Add(-2*time.Hour))
	files.PutObject(currentKey, []byte("current"), now.Add(-time.Hour))
	// A later re-upload may have written its object before its row commits;
	// it is newer than the current upload and must survive.
	files.PutObject(newerKey, []byte("newer"), now)
	svc := NewIngestService(store, repository.NewDocumentRepository(store),
		repository.NewChunkRepository(store), repository.NewIngestEventRepository(store),
		files, nil, nil, chunker.DefaultConfig())

	if err := svc.DeleteSupersededUploads(ctx, doc.ID, currentUpload); err != nil {
		t.Fatalf("DeleteSupersededUploads: %v", err)
	}
	keys := files.Keys()
	if len(keys) != 2 || !slices.Contains(keys, currentKey) || !slices.Contains(keys, newerKey) {
		t.Errorf("store holds %v, want the current upload %q and the newer one %q kept, the older %q deleted",
			keys, currentKey, newerKey, staleKey)
	}

	// A stale run must not delete the current upload's objects.
	files.PutObject(staleKey, []byte("stale again"), now)
	err := svc.DeleteSupersededUploads(ctx, doc.ID, staleUpload)
	var superseded *types.SupersededUploadError
	if !errors.As(err, &superseded) {
		t.Fatalf("DeleteSupersededUploads of a stale upload = %v, want *types.SupersededUploadError", err)
	}
	if keys := files.Keys(); len(keys) != 3 || !slices.Contains(keys, currentKey) ||
		!slices.Contains(keys, staleKey) || !slices.Contains(keys, newerKey) {
		t.Errorf("store holds %v, want every object kept after the superseded guard", keys)
	}

	var missing *types.DocumentNotFoundError
	if err := svc.DeleteSupersededUploads(ctx, uuid.New(), currentUpload); !errors.As(err, &missing) {
		t.Errorf("DeleteSupersededUploads(unknown Document) = %v, want *types.DocumentNotFoundError", err)
	}
}

func TestDeleteSupersededUploadsLeavesEverythingWhenTheCurrentUploadHasNoObjects(t *testing.T) {
	store := db.NewStore(dbtest.Migrated(t))
	_, owner := principalContext(t, store, "alice")
	doc := ingestDocument(t, store, owner.ID)
	staleUpload := uuid.New()
	files := filetest.NewMemory()
	staleKey := file.SourceKey(doc.ID, staleUpload)
	files.PutObject(staleKey, []byte("stale"), time.Now().Add(-2*time.Hour))
	svc := NewIngestService(store, repository.NewDocumentRepository(store),
		repository.NewChunkRepository(store), repository.NewIngestEventRepository(store),
		files, nil, nil, chunker.DefaultConfig())

	ctx, logs := observedContext()
	if err := svc.DeleteSupersededUploads(ctx, doc.ID, doc.UploadID); err != nil {
		t.Fatalf("DeleteSupersededUploads: %v", err)
	}
	if keys := files.Keys(); len(keys) != 1 || keys[0] != staleKey {
		t.Errorf("store holds %v, want the replaced upload kept when the current upload has no objects", keys)
	}
	if warns := logs.FilterMessage("ingest: current upload has no objects; leaving replaced uploads"); warns.Len() != 1 {
		t.Errorf("warnings = %v, want one about the missing current upload objects", logs.All())
	}
}
