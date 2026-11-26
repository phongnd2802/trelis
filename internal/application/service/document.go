// Package service holds Trelis's use cases, between the HTTP handlers and
// the repositories, in the layering Omnisc uses (handler → service →
// repository).
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/db"
	werrors "github.com/phongnd2802/trelis/internal/errors"
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
}

// NewDocumentService returns the Document use cases.
func NewDocumentService(store db.Store, repo interfaces.DocumentRepository,
	principals interfaces.PrincipalRepository, fileSvc interfaces.FileService,
) interfaces.DocumentService {
	return &documentService{store: store, repo: repo, principals: principals, fileSvc: fileSvc}
}

// CreateDocumentFromFile creates a pending Document from an uploaded file.
// The order follows Omnisc's CreateKnowledgeFromFile: check the type, hash
// for deduplication, validate the name, save the file, then the record,
// removing the file again if the record cannot be saved. No ingest is
// started here (#21).
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
			slog.ErrorContext(ctx, "delete source file after document creation failed", "key", key, "err", deleteErr)
		}
		return nil, s.createErr(ctx, owner.ID, hash, err)
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
		slog.ErrorContext(ctx, "source file missing from object storage", "document", doc.ID, "key", doc.ObjectKey)
		return nil, nil, errDocumentNotFound()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open source file of document %s: %w", doc.ID, err)
	}
	return doc, body, nil
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
