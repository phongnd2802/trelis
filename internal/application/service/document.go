// Package service holds Trelis's use cases, between the HTTP handlers and
// the repositories, in the layering Omnisc uses (handler → service →
// repository).
package service

import (
	"context"
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
	secutils "github.com/phongnd2802/trelis/internal/utils"
)

// documentService implements DocumentService over the Document and
// Principal repositories and file storage. It owns the transactions its use
// cases need (ADR-0007).
type documentService struct {
	store      db.Store
	repo       interfaces.DocumentRepository
	principals interfaces.PrincipalRepository
	fileSvc    interfaces.FileService
	ingest     interfaces.IngestStarter
	cleanup    interfaces.DocumentDeleteStarter
}

// NewDocumentService returns the Document use cases.
func NewDocumentService(store db.Store, repo interfaces.DocumentRepository,
	principals interfaces.PrincipalRepository, fileSvc interfaces.FileService,
	ingest interfaces.IngestStarter, cleanup interfaces.DocumentDeleteStarter,
) interfaces.DocumentUseCases {
	return &documentService{store: store, repo: repo, principals: principals, fileSvc: fileSvc,
		ingest: ingest, cleanup: cleanup}
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
	fileName := file.Filename
	fileType := getFileType(fileName)

	if err := validateImportFileType(fileType); err != nil {
		return nil, err
	}
	if file.Size == 0 {
		return nil, werrors.NewBadRequestError("File is empty")
	}

	// Calculate file hash for deduplication
	hash, err := calculateFileHash(file)
	if err != nil {
		return nil, fmt.Errorf("hash %q: %w", fileName, err)
	}
	existing, err := s.repo.GetDocumentByOwnerAndHash(ctx, owner.ID, hash)
	if err == nil {
		return existing, types.NewDuplicateDocumentError(existing)
	}
	if !errors.Is(err, repository.ErrDocumentNotFound) {
		return nil, err
	}

	safeFilename, isValid := secutils.ValidateInput(fileName)
	if !isValid || safeFilename == "" {
		return nil, werrors.NewValidationError("File name contains illegal characters")
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
		if deleteErr := s.fileSvc.DeleteFile(context.WithoutCancel(ctx), key); deleteErr != nil {
			logger.Ctx(ctx).Error("delete source file after document creation failed",
				zap.String("key", key), zap.Error(deleteErr))
		}
		return nil, s.createErr(ctx, owner.ID, hash, err)
	}
	// Starting is not part of the transaction (ADR-0005): a failure here
	// leaves a pending Document that the reconciler restarts, so the upload
	// still succeeds.
	if err := s.ingest.StartIngest(ctx, doc.ID); err != nil {
		logger.Ctx(ctx).Warn("start ingest workflow failed",
			zap.Stringer("document", doc.ID), zap.Error(err))
	}
	if doc.AccessList, err = s.repo.GetAccessList(ctx, doc.ID); err != nil {
		return nil, err
	}
	return doc, nil
}

// createErr maps a failed insert to what the client should see.
func (s *documentService) createErr(ctx context.Context, owner uuid.UUID, hash string, err error) error {
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
	if err := s.repo.DeleteDocument(ctx, id); err != nil {
		if errors.Is(err, repository.ErrDocumentNotFound) {
			return errDocumentNotFound()
		}
		return err
	}
	// Starting is not part of the row's transaction (ADR-0005): the row is
	// already gone, and the reconciler removes the prefix if this start fails.
	if err := s.cleanup.StartDocumentDelete(ctx, id); err != nil {
		logger.Ctx(ctx).Warn("start document delete workflow failed",
			zap.Stringer("document", id), zap.Error(err))
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
