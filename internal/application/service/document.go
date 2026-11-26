// Package service holds Trelis's use cases, between the HTTP handlers and
// the repositories, in the layering Omnisc uses (handler → service →
// repository).
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/auth"
	werrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
	secutils "github.com/phongnd2802/trelis/internal/utils"
)

// documentService implements DocumentService over a repository and file
// storage.
type documentService struct {
	repo    interfaces.DocumentRepository
	fileSvc interfaces.FileService
}

// NewDocumentService returns the Document use cases.
func NewDocumentService(repo interfaces.DocumentRepository, fileSvc interfaces.FileService) interfaces.DocumentService {
	return &documentService{repo: repo, fileSvc: fileSvc}
}

// caller is the Principal a request was authenticated as. Every route behind
// the router's authenticate middleware has one.
func caller(ctx context.Context) (auth.Principal, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return auth.Principal{}, errors.New("no authenticated principal in context")
	}
	return p, nil
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
		ParseStatus: types.ParseStatusPending,
	}

	// Save the file to storage
	filePath, err := s.fileSvc.SaveFile(ctx, file, doc.ID)
	if err != nil {
		return nil, fmt.Errorf("save file for document %s: %w", doc.ID, err)
	}
	doc.FilePath = filePath

	// Save the Document record after the file is safely stored.
	if err := s.repo.CreateDocument(ctx, doc, accessList); err != nil {
		if deleteErr := s.fileSvc.DeleteFile(context.WithoutCancel(ctx), filePath); deleteErr != nil {
			slog.ErrorContext(ctx, "delete file after document creation failed", "path", filePath, "err", deleteErr)
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

// GetDocumentByID returns a Document the caller can read, with its Access
// List when the caller owns it.
func (s *documentService) GetDocumentByID(ctx context.Context, id uuid.UUID) (*types.Document, error) {
	p, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	doc, err := s.repo.GetReadableDocumentByID(ctx, p.ID, id)
	if errors.Is(err, repository.ErrDocumentNotFound) {
		return nil, werrors.NewNotFoundError("Document not found")
	}
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
