// Package service holds Trelis's use cases, between the HTTP handlers and
// the repositories, in the layering Omnisc uses (handler → service →
// repository).
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/application/service/file"
	"github.com/phongnd2802/trelis/internal/db"
	werrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// documentService implements DocumentService over the Document and
// Principal repositories and file storage. It owns the transactions its use
// cases need (ADR-0007).
type documentService struct {
	store      db.Store
	repo       interfaces.DocumentRepository
	principals interfaces.PrincipalRepository
	events     interfaces.IngestEventRepository
	chunks     interfaces.ChunkRepository
	fileSvc    interfaces.FileService
	ingest     interfaces.IngestStarter
	cleanup    interfaces.DocumentDeleteStarter
}

// NewDocumentService returns the Document use cases.
func NewDocumentService(store db.Store, repo interfaces.DocumentRepository,
	principals interfaces.PrincipalRepository, events interfaces.IngestEventRepository,
	chunks interfaces.ChunkRepository, fileSvc interfaces.FileService,
	ingest interfaces.IngestStarter, cleanup interfaces.DocumentDeleteStarter,
) interfaces.DocumentUseCases {
	return &documentService{store: store, repo: repo, principals: principals, events: events,
		chunks: chunks, fileSvc: fileSvc, ingest: ingest, cleanup: cleanup}
}

// CreateDocumentFromFile creates a pending Document from an uploaded file and
// starts its ingest workflow. The order follows Omnisc's
// CreateKnowledgeFromFile: check the type, hash for deduplication, validate
// the name, save the file, then the record, removing the file again if the
// record cannot be saved. A workflow that will not start is a warning, not a
// failed upload: the Document stays pending for the reconciler to start on
// one of its passes (reconciler.interval).
func (s *documentService) CreateDocumentFromFile(ctx context.Context,
	file *multipart.FileHeader, accessList types.AccessList,
) (*types.Document, error) {
	owner, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	fileType, hash, safeFilename, err := validateUploadFile(file)
	if err != nil {
		return nil, err
	}
	existing, err := s.repo.GetDocumentByOwnerAndHash(ctx, owner.ID, hash)
	if err == nil {
		return existing, types.NewDuplicateDocumentError(existing)
	}
	if !errors.Is(err, repository.ErrDocumentNotFound) {
		return nil, err
	}

	doc := &types.Document{
		ID:          uuid.New(),
		OwnerID:     owner.ID,
		Title:       safeFilename,
		FileName:    safeFilename,
		FileType:    fileType,
		FileSize:    file.Size,
		FileHash:    hash,
		UploadID:    uuid.New(),
		ParseStatus: types.ParseStatusPending,
	}

	// Save the Source File to object storage under this upload's key.
	key, err := s.fileSvc.SaveFile(ctx, file, doc.ID, doc.UploadID)
	if err != nil {
		return nil, fmt.Errorf("save source file for document %s: %w", doc.ID, err)
	}
	doc.ObjectKey = key

	// Save the Document and its Access List after the file is safely stored,
	// in one transaction, so an unknown name leaves no Document behind.
	err = s.store.ExecTx(ctx, func(ctx context.Context) error {
		grants, err := s.principals.ResolveNames(ctx, accessList)
		if err != nil {
			return err
		}
		return s.repo.CreateDocument(ctx, doc, grants)
	})
	if err != nil {
		s.discardStoredFile(ctx, key, "document creation")
		return nil, s.uploadErr(ctx, owner.ID, hash, err)
	}
	// Starting is not part of the transaction (ADR-0005): a failure here
	// leaves a pending Document that the reconciler restarts, so the upload
	// still succeeds.
	s.startIngest(ctx, doc.ID, doc.UploadID)
	if doc.AccessList, err = s.repo.GetAccessList(ctx, doc.ID); err != nil {
		return nil, err
	}
	return doc, nil
}

// EnsureCanReplace checks that the caller can read the Document and owns it,
// with the same not-found and forbidden errors ReplaceDocumentFile answers. A
// handler calls it before reading an upload it would have to reject.
func (s *documentService) EnsureCanReplace(ctx context.Context, id uuid.UUID) error {
	_, _, err := s.replaceableDocument(ctx, id)
	return err
}

// ReplaceDocumentFile replaces a Document's Source File with file under a new
// upload ID, keeping its Access List. Only the owner may replace it: a reader
// who is not the owner gets a forbidden error, and a caller who cannot read
// the Document gets the same not-found error as for a missing one. The same
// content as the Document's current upload, or as another Document of the
// owner's, is a duplicate error carrying that Document.
//
// The file is stored first, then one transaction puts the new row values in
// place, deletes the old Chunks and Parent Chunks and writes upload_replaced.
// The transaction's update runs first and is conditional on the upload the
// caller read, so a concurrent re-upload or delete wins instead of being
// overwritten. A failed transaction deletes the new object again, as
// CreateDocumentFromFile does. After the commit the previous upload's ingest
// workflow is terminated and the new upload's is started, both outside the
// transaction (ADR-0005); a failure there is a warning, not a failed
// replacement, because the upload guard and the reconciler cover it.
func (s *documentService) ReplaceDocumentFile(ctx context.Context, id uuid.UUID, file *multipart.FileHeader) (*types.Document, error) {
	p, doc, err := s.replaceableDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	fileType, hash, safeFilename, err := validateUploadFile(file)
	if err != nil {
		return nil, err
	}
	existing, err := s.repo.GetDocumentByOwnerAndHash(ctx, p.ID, hash)
	if err == nil {
		return existing, types.NewDuplicateDocumentError(existing)
	}
	if !errors.Is(err, repository.ErrDocumentNotFound) {
		return nil, err
	}

	uploadID := uuid.New()
	detail, err := json.Marshal(types.UploadReplacedDetail{UploadID: uploadID})
	if err != nil {
		return nil, fmt.Errorf("encode upload_replaced event of document %s: %w", doc.ID, err)
	}
	key, err := s.fileSvc.SaveFile(ctx, file, doc.ID, uploadID)
	if err != nil {
		return nil, fmt.Errorf("save source file for document %s: %w", doc.ID, err)
	}
	updated := &types.Document{
		ID:          doc.ID,
		OwnerID:     doc.OwnerID,
		Title:       safeFilename,
		FileName:    safeFilename,
		FileType:    fileType,
		FileSize:    file.Size,
		FileHash:    hash,
		UploadID:    uploadID,
		ObjectKey:   key,
		ParseStatus: types.ParseStatusPending,
	}

	previousUpload := doc.UploadID
	err = s.store.ExecTx(ctx, func(ctx context.Context) error {
		replaced, err := s.repo.ReplaceDocumentUpload(ctx, updated, previousUpload)
		if err != nil {
			return err
		}
		if err := s.chunks.DeleteChunks(ctx, doc.ID); err != nil {
			return err
		}
		_, err = s.events.InsertIngestEvent(ctx, &types.IngestEvent{
			DocumentID: doc.ID,
			UploadID:   uploadID,
			Type:       types.IngestEventUploadReplaced,
			Detail:     detail,
		})
		if err != nil {
			return err
		}
		*updated = *replaced
		return nil
	})
	if err != nil {
		s.discardStoredFile(ctx, key, "document replacement")
		switch {
		case errors.Is(err, repository.ErrDocumentNotFound):
			return nil, errDocumentNotFound()
		case errors.Is(err, repository.ErrUploadChanged):
			return nil, werrors.NewPreconditionFailedError("Document changed while uploading; retry")
		default:
			return nil, s.uploadErr(ctx, p.ID, hash, err)
		}
	}
	// Terminating before starting keeps the old run from writing under the
	// new upload; the upload guard makes a failed terminate harmless, and a
	// failed start is picked up by the reconciler.
	if err := s.ingest.TerminateIngest(ctx, doc.ID, previousUpload); err != nil {
		logger.Ctx(ctx).Warn("terminate previous ingest workflow failed",
			zap.Stringer("document", doc.ID), zap.Stringer("upload", previousUpload), zap.Error(err))
	}
	s.startIngest(ctx, doc.ID, uploadID)
	if updated.AccessList, err = s.repo.GetAccessList(ctx, doc.ID); err != nil {
		return nil, err
	}
	return updated, nil
}

// uploadErr maps a failed upload transaction to what the client should see.
func (s *documentService) uploadErr(ctx context.Context, owner uuid.UUID, hash string, err error) error {
	switch {
	case errors.Is(err, repository.ErrPrincipalNotFound), errors.Is(err, repository.ErrGroupNotFound):
		return werrors.NewBadRequestError("Invalid access list: " + err.Error())
	case errors.Is(err, repository.ErrDuplicateDocument):
		// A concurrent upload of the same content won the race.
		existing, getErr := s.repo.GetDocumentByOwnerAndHash(ctx, owner, hash)
		if getErr != nil {
			return getErr
		}
		return types.NewDuplicateDocumentError(existing)
	default:
		return err
	}
}

// discardStoredFile deletes key after operation's transaction failed, so a
// rolled-back upload leaves no object behind. A failed delete is logged; the
// reconciler removes the orphan later.
func (s *documentService) discardStoredFile(ctx context.Context, key, operation string) {
	if err := s.fileSvc.DeleteFile(context.WithoutCancel(ctx), key); err != nil {
		logger.Ctx(ctx).Error("delete source file after "+operation+" failed",
			zap.String("key", key), zap.Error(err))
	}
}

// startIngest starts one upload's ingest workflow and warns when Temporal
// refuses it. The Document stays pending for the reconciler to submit again.
func (s *documentService) startIngest(ctx context.Context, documentID, uploadID uuid.UUID) {
	if err := s.ingest.StartIngest(ctx, documentID, uploadID); err != nil {
		logger.Ctx(ctx).Warn("start ingest workflow failed",
			zap.Stringer("document", documentID), zap.Stringer("upload", uploadID), zap.Error(err))
	}
}

// replaceableDocument returns the caller and the Document when the caller can
// read it and owns it, so a non-owner gets the forbidden error and a
// non-reader the same not-found error as for a missing Document.
func (s *documentService) replaceableDocument(ctx context.Context, id uuid.UUID) (types.Principal, *types.Document, error) {
	p, doc, err := s.readableDocument(ctx, id)
	if err != nil {
		return types.Principal{}, nil, err
	}
	if doc.OwnerID != p.ID {
		return types.Principal{}, nil, werrors.NewForbiddenError("Only the owner can replace the Document")
	}
	return p, doc, nil
}

// errDocumentNotFound is what a caller sees for a Document that is missing
// and for one they cannot read alike.
func errDocumentNotFound() error { return werrors.NewNotFoundError("Document not found") }

// readableDocument returns the caller and the Document if the caller can
// read it, else errDocumentNotFound.
func (s *documentService) readableDocument(ctx context.Context, id uuid.UUID) (types.Principal, *types.Document, error) {
	p, err := caller(ctx)
	if err != nil {
		return types.Principal{}, nil, err
	}
	doc, err := s.repo.GetReadableDocumentByID(ctx, p.ID, id)
	if errors.Is(err, repository.ErrDocumentNotFound) {
		return types.Principal{}, nil, errDocumentNotFound()
	}
	if err != nil {
		return types.Principal{}, nil, err
	}
	return p, doc, nil
}

// GetDocumentByID returns a Document the caller can read, with its Access
// List when the caller owns it.
func (s *documentService) GetDocumentByID(ctx context.Context, id uuid.UUID) (*types.Document, error) {
	p, doc, err := s.readableDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	if doc.OwnerID == p.ID {
		if doc.AccessList, err = s.repo.GetAccessList(ctx, doc.ID); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

// GetDocumentFile opens the Source File of a Document the caller can read.
// A Document they cannot read is the same not-found error as a missing one,
// and so is a row whose object is gone. The caller closes the reader.
func (s *documentService) GetDocumentFile(ctx context.Context, id uuid.UUID) (*types.Document, io.ReadCloser, error) {
	_, doc, err := s.readableDocument(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	body, err := s.fileSvc.GetFile(ctx, doc.ObjectKey)
	if errors.Is(err, interfaces.ErrFileNotFound) {
		logger.Ctx(ctx).Error("source file missing from object storage",
			zap.Stringer("document", doc.ID), zap.String("key", doc.ObjectKey))
		return nil, nil, errDocumentNotFound()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open source file of document %s: %w", doc.ID, err)
	}
	return doc, body, nil
}

// DeleteDocument removes a Document the caller owns; its Chunks, Parent
// Chunks and Access List go with the row by ON DELETE CASCADE. The Access
// List grants reading, but deleting is a write, so only the uploader may
// delete: a reader who is not the owner gets a forbidden error, and a caller
// who cannot read the Document gets the same not-found error as for a missing
// one. Once the row is gone it starts the cleanup workflow that removes every
// object under the Document's prefix (ADR-0006). A failed start is a warning,
// not a failed delete: the #26 reconciler deletes a prefix whose Document row
// is gone after its one-hour grace (orphanObjectGrace). An ingest that was not
// cancelled also ends on its own: its activities find no Document, and any
// Chunk insert fails on the foreign key.
func (s *documentService) DeleteDocument(ctx context.Context, id uuid.UUID) error {
	p, doc, err := s.readableDocument(ctx, id)
	if err != nil {
		return err
	}
	if doc.OwnerID != p.ID {
		return werrors.NewForbiddenError("Only the owner can delete the Document")
	}
	uploadID, err := s.repo.DeleteDocument(ctx, id)
	if errors.Is(err, repository.ErrDocumentNotFound) {
		return errDocumentNotFound()
	}
	if err != nil {
		return err
	}
	// Starting is not part of the row's transaction (ADR-0005): the row is
	// already gone, and the reconciler removes the prefix if this start fails.
	// The upload ID comes from the deleted row, so a re-upload that won the
	// race since the read still has its ingest cancelled.
	if err := s.cleanup.StartDocumentDelete(ctx, id, uploadID); err != nil {
		logger.Ctx(ctx).Warn("start document delete workflow failed",
			zap.Stringer("document", id), zap.Stringer("upload", uploadID), zap.Error(err))
	}
	return nil
}

// DeleteDocumentObjects removes every object under the Document's
// documents/{document_id}/ prefix. It is the use case the cleanup workflow's
// activity calls after the row is gone; an empty prefix is not an error, so a
// retried activity is harmless.
func (s *documentService) DeleteDocumentObjects(ctx context.Context, documentID uuid.UUID) error {
	if err := s.fileSvc.DeletePrefix(ctx, file.DocumentPrefix(documentID)); err != nil {
		return fmt.Errorf("delete objects of document %s: %w", documentID, err)
	}
	return nil
}

// ListPagedDocuments returns a page of the Documents the caller can read,
// without Access Lists.
func (s *documentService) ListPagedDocuments(ctx context.Context, page *types.Pagination) (*types.PageResult, error) {
	p, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	docs, total, err := s.repo.ListPagedReadableDocuments(ctx, p.ID, page)
	if err != nil {
		return nil, err
	}
	return types.NewPageResult(total, page, docs), nil
}

// IngestEventsAfter returns the Ingest Events of a Document the caller can
// read whose Seq is greater than afterSeq, in order, and the Document's
// current upload ID. The Access List is read on every call, so a revoked
// grant or a deleted Document ends an open progress stream on its next poll;
// a caller who cannot read it gets the same not-found error as for a missing
// Document (ADR-0004). The current upload is read after the events, so a
// stream that replays an older upload's terminal event sees the new upload
// and keeps going instead of stopping there.
func (s *documentService) IngestEventsAfter(ctx context.Context, id uuid.UUID, afterSeq int64) ([]*types.IngestEvent, uuid.UUID, error) {
	if _, _, err := s.readableDocument(ctx, id); err != nil {
		return nil, uuid.Nil, err
	}
	events, err := s.events.ListIngestEventsAfter(ctx, id, afterSeq)
	if err != nil {
		return nil, uuid.Nil, err
	}
	uploadID, err := s.repo.GetDocumentUploadID(ctx, id)
	if errors.Is(err, repository.ErrDocumentNotFound) {
		return nil, uuid.Nil, errDocumentNotFound()
	}
	if err != nil {
		return nil, uuid.Nil, err
	}
	return events, uploadID, nil
}
