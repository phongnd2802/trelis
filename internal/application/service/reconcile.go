package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/phongnd2802/trelis/internal/application/service/file"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// orphanObjectGrace is how long every object of a prefix must have gone
// untouched before the reconciler deletes it. An upload writes its object
// before its Document row (ADR-0006), and one S3 transfer may take up to 30
// minutes (objectStorageTransferTimeout), so a shorter grace could delete the
// source of an upload that has not committed its row yet. It is a fixed
// constant, not configuration: shortening it would turn that race into data
// loss.
const orphanObjectGrace = time.Hour

// uploadModTimes maps an upload ID to the newest last-modified time among
// that upload's objects.
type uploadModTimes map[uuid.UUID]time.Time

// documentModTimes maps a Document ID to each upload under its prefix.
type documentModTimes map[uuid.UUID]uploadModTimes

// reconcileService implements ReconcileService over the Document repository,
// object storage and the ingest starter.
type reconcileService struct {
	documents interfaces.DocumentRepository
	files     interfaces.FileService
	ingest    interfaces.IngestStarter
	// now is the clock the age checks use; tests replace it.
	now func() time.Time
}

// NewReconcileService returns the reconciler's single pass. The runner in
// cmd/server calls it on a ticker; every step is idempotent, so replicas may
// run a pass at the same time.
func NewReconcileService(documents interfaces.DocumentRepository, files interfaces.FileService,
	ingest interfaces.IngestStarter,
) interfaces.ReconcileService {
	return &reconcileService{documents: documents, files: files, ingest: ingest, now: time.Now}
}

// Reconcile runs both parts of one pass. They are independent, so one
// failing does not skip the other, and the returned error joins whatever
// failed.
func (s *reconcileService) Reconcile(ctx context.Context) error {
	return errors.Join(s.restartPending(ctx), s.deleteOrphans(ctx))
}

// restartPending submits the start of every pending Document's ingest
// workflow. The workflow ID carries the Document and upload IDs, so a
// Document whose run is already open is joined instead of duplicated
// (ADR-0005) and no check against Temporal is needed here. One Document that
// will not start is skipped and the rest still run; the returned error counts
// the failures and carries the first cause.
func (s *reconcileService) restartPending(ctx context.Context) error {
	pending, err := s.documents.ListPendingDocuments(ctx)
	if err != nil {
		return fmt.Errorf("list pending documents: %w", err)
	}
	if len(pending) == 0 {
		logger.Ctx(ctx).Debug("reconciler: no pending documents")
		return nil
	}
	submitted, failed := 0, 0
	var firstErr error
	for _, doc := range pending {
		if err := s.ingest.StartIngest(ctx, doc.DocumentID, doc.UploadID); err != nil {
			logger.Ctx(ctx).Debug("reconciler: start ingest failed",
				zap.Stringer("document", doc.DocumentID), zap.Stringer("upload", doc.UploadID), zap.Error(err))
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		submitted++
	}
	if submitted > 0 {
		logger.Ctx(ctx).Debug("reconciler: submitted pending documents", zap.Int("documents", submitted))
	}
	if failed > 0 {
		return fmt.Errorf("start ingest failed for %d of %d pending documents: %w", failed, len(pending), firstErr)
	}
	return nil
}

// deleteOrphans deletes every object prefix that no Document or no current
// upload of one refers to. A replaced upload is deleted only once the
// Document has completed or failed: before that, its new upload's Chunks may
// not be committed yet, and the ingest workflow's delete_superseded_uploads
// step removes the previous upload once they are. A prefix is deleted only
// once every one of its objects is older than orphanObjectGrace, so a young
// object may be an upload that has not written its row or new upload_id yet.
// Keys that do not follow the documents/{document_id}/{upload_id}/... layout
// are left alone and reported together.
func (s *reconcileService) deleteOrphans(ctx context.Context) error {
	objects, err := s.files.ListObjects(ctx, file.DocumentsPrefix)
	if err != nil {
		return fmt.Errorf("list objects under %s: %w", file.DocumentsPrefix, err)
	}
	uploadsByDocument := documentModTimes{}
	unrecognised, unrecognisedExample := 0, ""
	for _, object := range objects {
		documentID, uploadID, err := file.ParseKey(object.Key)
		if err != nil {
			unrecognised++
			if unrecognisedExample == "" {
				unrecognisedExample = object.Key
			}
			continue
		}
		uploads := uploadsByDocument[documentID]
		if uploads == nil {
			uploads = uploadModTimes{}
			uploadsByDocument[documentID] = uploads
		}
		if newest, ok := uploads[uploadID]; !ok || object.LastModified.After(newest) {
			uploads[uploadID] = object.LastModified
		}
	}
	if unrecognised > 0 {
		logger.Ctx(ctx).Warn("reconciler: left objects with unrecognised keys",
			zap.Int("objects", unrecognised), zap.String("example", unrecognisedExample))
	}
	if len(uploadsByDocument) == 0 {
		logger.Ctx(ctx).Debug("reconciler: no objects to reconcile")
		return nil
	}

	documentIDs := slices.Collect(maps.Keys(uploadsByDocument))
	currentUploads, err := s.documents.ListDocumentUploads(ctx, documentIDs)
	if err != nil {
		return fmt.Errorf("list current document uploads: %w", err)
	}

	now := s.now()
	deleted, attempted, failed := 0, 0, 0
	var firstErr error
	deletePrefix := func(prefix string, lastModified time.Time) {
		if now.Sub(lastModified) < orphanObjectGrace {
			return
		}
		attempted++
		if err := s.files.DeletePrefix(ctx, prefix); err != nil {
			logger.Ctx(ctx).Debug("reconciler: delete orphaned objects failed",
				zap.String("prefix", prefix), zap.Error(err))
			failed++
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		deleted++
	}
	for _, documentID := range documentIDs {
		uploads := uploadsByDocument[documentID]
		currentUpload, exists := currentUploads[documentID]
		if !exists {
			// Deleting the Document prefix covers every upload; judge it by
			// the newest object under any of them.
			deletePrefix(file.DocumentPrefix(documentID), newestModTime(uploads))
			continue
		}
		// ADR-0006 deletes a previous upload's objects only after the new
		// version's Chunks are committed. A pending or processing Document
		// may still be committing them; its ingest workflow's
		// delete_superseded_uploads step removes the previous upload once
		// ParseAndChunk has committed. The reconciler is the backstop for a
		// Document that finished.
		if currentUpload.ParseStatus != types.ParseStatusCompleted &&
			currentUpload.ParseStatus != types.ParseStatusFailed {
			continue
		}
		for uploadID, lastModified := range uploads {
			if uploadID == currentUpload.UploadID {
				continue
			}
			deletePrefix(file.UploadPrefix(documentID, uploadID), lastModified)
		}
	}
	if deleted > 0 {
		logger.Ctx(ctx).Info("reconciler: deleted orphaned objects", zap.Int("prefixes", deleted))
	} else {
		logger.Ctx(ctx).Debug("reconciler: no orphaned objects deleted", zap.Int("objects", len(objects)))
	}
	if failed > 0 {
		return fmt.Errorf("delete orphaned objects failed for %d of %d prefixes: %w", failed, attempted, firstErr)
	}
	return nil
}

// newestModTime is the most recent last-modified time among uploads, so a
// prefix is deleted only when every object under it has outlived the grace
// period.
func newestModTime(uploads uploadModTimes) time.Time {
	var newest time.Time
	for _, modTime := range uploads {
		if modTime.After(newest) {
			newest = modTime
		}
	}
	return newest
}
