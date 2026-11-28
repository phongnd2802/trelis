package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/application/service/file"
	"github.com/phongnd2802/trelis/internal/application/service/file/filetest"
	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// fakeReconcileDocuments answers the two queries the reconciler runs; every
// other DocumentRepository method is left to the embedded nil.
type fakeReconcileDocuments struct {
	interfaces.DocumentRepository
	pending    []types.PendingDocument
	pendingErr error
	uploads    map[uuid.UUID]types.DocumentUpload
	uploadsErr error
	askedIDs   []uuid.UUID
	uploadRuns int
}

func (f *fakeReconcileDocuments) ListPendingDocuments(context.Context) ([]types.PendingDocument, error) {
	return f.pending, f.pendingErr
}

func (f *fakeReconcileDocuments) ListDocumentUploads(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]types.DocumentUpload, error) {
	f.uploadRuns++
	f.askedIDs = slices.Clone(ids)
	return f.uploads, f.uploadsErr
}

// fakeReconcileStarter records the Documents and uploads it was asked to
// submit and fails the Documents in fail.
type fakeReconcileStarter struct {
	submitted []types.PendingDocument
	fail      map[uuid.UUID]error
}

func (f *fakeReconcileStarter) StartIngest(_ context.Context, documentID, uploadID uuid.UUID) error {
	f.submitted = append(f.submitted, types.PendingDocument{DocumentID: documentID, UploadID: uploadID})
	return f.fail[documentID]
}

func (f *fakeReconcileStarter) TerminateIngest(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

// failingDeleteFiles is a Memory whose prefix delete always fails.
type failingDeleteFiles struct {
	*filetest.Memory
	err error
}

func (f *failingDeleteFiles) DeletePrefix(context.Context, string) error { return f.err }

// reconcileNow is the clock the reconciler tests run at.
var reconcileNow = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

// testReconciler returns the service with a frozen clock.
func testReconciler(documents interfaces.DocumentRepository, files interfaces.FileService,
	ingest interfaces.IngestStarter,
) *reconcileService {
	return &reconcileService{
		documents: documents,
		files:     files,
		ingest:    ingest,
		now:       func() time.Time { return reconcileNow },
	}
}

// observedContext is a context whose logs a test can read.
func observedContext() (context.Context, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return logger.WithContext(context.Background(), zap.New(core)), logs
}

func TestReconcileSubmitsOnlyPendingDocuments(t *testing.T) {
	ctx := context.Background()
	store := db.NewStore(dbtest.Migrated(t))
	owner, err := store.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	documents := repository.NewDocumentRepository(store)
	pending := ingestDocument(t, store, owner.ID)
	for _, status := range []string{
		types.ParseStatusProcessing,
		types.ParseStatusCompleted,
		types.ParseStatusFailed,
	} {
		doc := ingestDocument(t, store, owner.ID)
		if err := documents.SetParseStatus(ctx, doc.ID, status, ""); err != nil {
			t.Fatalf("SetParseStatus(%s): %v", status, err)
		}
	}
	starter := &fakeReconcileStarter{}
	svc := testReconciler(documents, filetest.NewMemory(), starter)

	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []types.PendingDocument{{
		DocumentID: pending.ID,
		UploadID:   pending.UploadID,
	}}
	if !slices.Equal(starter.submitted, want) {
		t.Errorf("StartIngest got %v, want only the pending Document's upload %v", starter.submitted, want)
	}
}

func TestReconcileReportsAFailedSubmissionAndSubmitsTheOthers(t *testing.T) {
	failing, healthy := types.PendingDocument{DocumentID: uuid.New(), UploadID: uuid.New()},
		types.PendingDocument{DocumentID: uuid.New(), UploadID: uuid.New()}
	startErr := errors.New("temporal unavailable")
	documents := &fakeReconcileDocuments{
		pending: []types.PendingDocument{failing, healthy},
		uploads: map[uuid.UUID]types.DocumentUpload{},
	}
	starter := &fakeReconcileStarter{fail: map[uuid.UUID]error{failing.DocumentID: startErr}}
	ctx, logs := observedContext()
	svc := testReconciler(documents, filetest.NewMemory(), starter)

	err := svc.Reconcile(ctx)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 pending documents") ||
		!strings.Contains(err.Error(), startErr.Error()) {
		t.Fatalf("Reconcile = %v, want one error counting the failure and naming its cause", err)
	}
	if want := []types.PendingDocument{failing, healthy}; !slices.Equal(starter.submitted, want) {
		t.Errorf("StartIngest got %v, want both Documents despite the failure %v", starter.submitted, want)
	}
	if warns := logs.FilterLevelExact(zapcore.WarnLevel); warns.Len() != 0 {
		t.Errorf("the service logged %d warnings, want the runner to be the only WARN: %v", warns.Len(), warns.All())
	}
}

func TestReconcileReportsDeleteFailuresAsOneError(t *testing.T) {
	firstDocument, secondDocument := uuid.New(), uuid.New()
	files := filetest.NewMemory()
	for _, documentID := range []uuid.UUID{firstDocument, secondDocument} {
		key := file.SourceKey(documentID, uuid.New())
		files.PutObject(key, []byte("orphan"), reconcileNow.Add(-2*time.Hour))
	}
	deleteErr := errors.New("s3 unavailable")
	documents := &fakeReconcileDocuments{uploads: map[uuid.UUID]types.DocumentUpload{}}
	ctx, logs := observedContext()
	svc := testReconciler(documents,
		&failingDeleteFiles{Memory: files, err: deleteErr}, &fakeReconcileStarter{})

	err := svc.Reconcile(ctx)
	if err == nil || !strings.Contains(err.Error(), "2 of 2 prefixes") ||
		!strings.Contains(err.Error(), deleteErr.Error()) {
		t.Fatalf("Reconcile = %v, want one error counting the failures and naming the cause", err)
	}
	if warns := logs.FilterLevelExact(zapcore.WarnLevel); warns.Len() != 0 {
		t.Errorf("the service logged %d warnings, want the runner to be the only WARN: %v", warns.Len(), warns.All())
	}
}

func TestReconcileDeletesAnOrphanedDocumentPrefix(t *testing.T) {
	documentID, uploadID := uuid.New(), uuid.New()
	files := filetest.NewMemory()
	key := file.SourceKey(documentID, uploadID)
	files.PutObject(key, []byte("orphan"), reconcileNow.Add(-2*time.Hour))
	documents := &fakeReconcileDocuments{uploads: map[uuid.UUID]types.DocumentUpload{}}
	svc := testReconciler(documents, files, &fakeReconcileStarter{})

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if keys := files.Keys(); len(keys) != 0 {
		t.Errorf("store still holds %v, want the prefix of the missing Document deleted", keys)
	}
	if want := []uuid.UUID{documentID}; !slices.Equal(documents.askedIDs, want) {
		t.Errorf("ListDocumentUploads asked for %v, want %v once", documents.askedIDs, want)
	}
}

func TestReconcileDeletesAStaleUploadAndKeepsTheCurrentOne(t *testing.T) {
	documentID, currentUpload, staleUpload := uuid.New(), uuid.New(), uuid.New()
	files := filetest.NewMemory()
	currentKey := file.SourceKey(documentID, currentUpload)
	staleKey := file.SourceKey(documentID, staleUpload)
	// The current upload is old too: only a replaced upload is ever deleted.
	files.PutObject(currentKey, []byte("current"), reconcileNow.Add(-3*time.Hour))
	files.PutObject(staleKey, []byte("stale"), reconcileNow.Add(-2*time.Hour))
	documents := &fakeReconcileDocuments{uploads: map[uuid.UUID]types.DocumentUpload{documentID: {
		DocumentID:  documentID,
		UploadID:    currentUpload,
		ParseStatus: types.ParseStatusCompleted,
	}}}
	svc := testReconciler(documents, files, &fakeReconcileStarter{})

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if keys := files.Keys(); !slices.Equal(keys, []string{currentKey}) {
		t.Errorf("store holds %v, want only the current upload %q", keys, currentKey)
	}
}

func TestReconcileKeepsAStaleUploadUntilTheNewVersionFinishes(t *testing.T) {
	for _, status := range []string{types.ParseStatusPending, types.ParseStatusProcessing} {
		t.Run(status, func(t *testing.T) {
			documentID, currentUpload, staleUpload := uuid.New(), uuid.New(), uuid.New()
			files := filetest.NewMemory()
			currentKey := file.SourceKey(documentID, currentUpload)
			staleKey := file.SourceKey(documentID, staleUpload)
			files.PutObject(currentKey, []byte("current"), reconcileNow.Add(-3*time.Hour))
			files.PutObject(staleKey, []byte("stale"), reconcileNow.Add(-2*time.Hour))
			documents := &fakeReconcileDocuments{uploads: map[uuid.UUID]types.DocumentUpload{documentID: {
				DocumentID:  documentID,
				UploadID:    currentUpload,
				ParseStatus: status,
			}}}
			svc := testReconciler(documents, files, &fakeReconcileStarter{})

			if err := svc.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			want := []string{currentKey, staleKey}
			slices.Sort(want)
			if keys := files.Keys(); !slices.Equal(keys, want) {
				t.Errorf("store holds %v, want the replaced upload kept while the Document is %s %v",
					keys, status, want)
			}
		})
	}
}

func TestReconcileDeletesAStaleUploadOnceTheNewVersionFailed(t *testing.T) {
	documentID, currentUpload, staleUpload := uuid.New(), uuid.New(), uuid.New()
	files := filetest.NewMemory()
	currentKey := file.SourceKey(documentID, currentUpload)
	staleKey := file.SourceKey(documentID, staleUpload)
	files.PutObject(currentKey, []byte("current"), reconcileNow.Add(-3*time.Hour))
	files.PutObject(staleKey, []byte("stale"), reconcileNow.Add(-2*time.Hour))
	documents := &fakeReconcileDocuments{uploads: map[uuid.UUID]types.DocumentUpload{documentID: {
		DocumentID:  documentID,
		UploadID:    currentUpload,
		ParseStatus: types.ParseStatusFailed,
	}}}
	svc := testReconciler(documents, files, &fakeReconcileStarter{})

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if keys := files.Keys(); !slices.Equal(keys, []string{currentKey}) {
		t.Errorf("store holds %v, want only the current upload %q", keys, currentKey)
	}
}

func TestReconcileKeepsAnUploadPrefixWithOneFreshObject(t *testing.T) {
	documentID, currentUpload, staleUpload := uuid.New(), uuid.New(), uuid.New()
	files := filetest.NewMemory()
	currentKey := file.SourceKey(documentID, currentUpload)
	staleSource := file.SourceKey(documentID, staleUpload)
	staleFigure := file.UploadPrefix(documentID, staleUpload) + "figures/1.png"
	files.PutObject(currentKey, []byte("current"), reconcileNow.Add(-3*time.Hour))
	files.PutObject(staleSource, []byte("old source"), reconcileNow.Add(-2*time.Hour))
	files.PutObject(staleFigure, []byte("fresh figure"), reconcileNow.Add(-10*time.Minute))
	documents := &fakeReconcileDocuments{uploads: map[uuid.UUID]types.DocumentUpload{documentID: {
		DocumentID:  documentID,
		UploadID:    currentUpload,
		ParseStatus: types.ParseStatusCompleted,
	}}}
	svc := testReconciler(documents, files, &fakeReconcileStarter{})

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{currentKey, staleFigure, staleSource}
	slices.Sort(want)
	if keys := files.Keys(); !slices.Equal(keys, want) {
		t.Errorf("store holds %v, want the stale upload kept while one of its objects is fresh %v", keys, want)
	}
}

func TestReconcileKeepsAMissingDocumentWithAFreshObjectInASecondUpload(t *testing.T) {
	documentID := uuid.New()
	firstUpload, secondUpload := uuid.New(), uuid.New()
	files := filetest.NewMemory()
	firstKey := file.SourceKey(documentID, firstUpload)
	secondSource := file.SourceKey(documentID, secondUpload)
	secondFigure := file.UploadPrefix(documentID, secondUpload) + "figures/1.png"
	files.PutObject(firstKey, []byte("old first upload"), reconcileNow.Add(-2*time.Hour))
	files.PutObject(secondSource, []byte("old second upload"), reconcileNow.Add(-2*time.Hour))
	files.PutObject(secondFigure, []byte("fresh figure"), reconcileNow.Add(-10*time.Minute))
	documents := &fakeReconcileDocuments{uploads: map[uuid.UUID]types.DocumentUpload{}}
	svc := testReconciler(documents, files, &fakeReconcileStarter{})

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{firstKey, secondFigure, secondSource}
	slices.Sort(want)
	if keys := files.Keys(); !slices.Equal(keys, want) {
		t.Errorf("store holds %v, want the missing Document kept while any object is fresh %v", keys, want)
	}
}

func TestReconcileKeepsPrefixesYoungerThanTheGracePeriod(t *testing.T) {
	orphanDocument, orphanUpload := uuid.New(), uuid.New()
	documentID, currentUpload, staleUpload := uuid.New(), uuid.New(), uuid.New()
	files := filetest.NewMemory()
	young := reconcileNow.Add(-30 * time.Minute)
	orphanKey := file.SourceKey(orphanDocument, orphanUpload)
	currentKey := file.SourceKey(documentID, currentUpload)
	staleKey := file.SourceKey(documentID, staleUpload)
	for _, key := range []string{orphanKey, currentKey, staleKey} {
		files.PutObject(key, []byte(key), young)
	}
	documents := &fakeReconcileDocuments{uploads: map[uuid.UUID]types.DocumentUpload{documentID: {
		DocumentID:  documentID,
		UploadID:    currentUpload,
		ParseStatus: types.ParseStatusCompleted,
	}}}
	svc := testReconciler(documents, files, &fakeReconcileStarter{})

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{currentKey, orphanKey, staleKey}
	slices.Sort(want)
	if keys := files.Keys(); !slices.Equal(keys, want) {
		t.Errorf("store holds %v, want every young prefix kept %v", keys, want)
	}
}

func TestReconcileLeavesAKeyItCannotParse(t *testing.T) {
	files := filetest.NewMemory()
	key := "documents/not-a-document/not-an-upload/source"
	files.PutObject(key, []byte("unparseable"), reconcileNow.Add(-2*time.Hour))
	documents := &fakeReconcileDocuments{uploads: map[uuid.UUID]types.DocumentUpload{}}
	svc := testReconciler(documents, files, &fakeReconcileStarter{})

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if keys := files.Keys(); !slices.Equal(keys, []string{key}) {
		t.Errorf("store holds %v, want the unparseable key kept", keys)
	}
	if documents.uploadRuns != 0 {
		t.Errorf("ListDocumentUploads ran %d times for no parsed Document, want none", documents.uploadRuns)
	}
}

func TestReconcileLogsOneSummaryForUnrecognisedKeys(t *testing.T) {
	firstKey, secondKey := "documents/bad-1/x/source", "documents/bad-2/x/source"
	files := filetest.NewMemory()
	for _, key := range []string{firstKey, secondKey} {
		files.PutObject(key, []byte("unparseable"), reconcileNow.Add(-2*time.Hour))
	}
	documents := &fakeReconcileDocuments{uploads: map[uuid.UUID]types.DocumentUpload{}}
	ctx, logs := observedContext()
	svc := testReconciler(documents, files, &fakeReconcileStarter{})

	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	entries := logs.FilterMessage("reconciler: left objects with unrecognised keys").All()
	if len(entries) != 1 {
		t.Fatalf("logged %d unrecognised-key lines, want one summary: %v", len(entries), logs.All())
	}
	if entries[0].Level != zapcore.WarnLevel {
		t.Errorf("summary level = %v, want WARN", entries[0].Level)
	}
	fields := entries[0].ContextMap()
	if fields["objects"] != int64(2) {
		t.Errorf("summary count = %v, want 2", fields["objects"])
	}
	if fields["example"] != firstKey {
		t.Errorf("summary example = %v, want %q", fields["example"], firstKey)
	}
	if warns := logs.FilterLevelExact(zapcore.WarnLevel); warns.Len() != 1 {
		t.Errorf("logged %d warnings, want only the one summary: %v", warns.Len(), warns.All())
	}
}
