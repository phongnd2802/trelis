package interfaces

import (
	"context"
	"io"
	"mime/multipart"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// DocumentService is the Document use cases, always on behalf of the
// Principal in ctx (auth.NewContext). It answers with *errors.AppError for
// anything the client should see, and *types.DuplicateDocumentError when the
// caller already uploaded the same content.
type DocumentService interface {
	// CreateDocumentFromFile stores the file as a pending Document owned by
	// the caller, readable by the caller and everyone on accessList.
	CreateDocumentFromFile(ctx context.Context, file *multipart.FileHeader, accessList types.AccessList) (*types.Document, error)
	// GetDocumentByID returns a Document the caller can read; one that does
	// not exist and one they cannot read are the same not-found error.
	GetDocumentByID(ctx context.Context, id uuid.UUID) (*types.Document, error)
	// GetDocumentFile opens the Source File of a Document the caller can
	// read, with the same not-found error as GetDocumentByID. The caller
	// closes the reader.
	GetDocumentFile(ctx context.Context, id uuid.UUID) (*types.Document, io.ReadCloser, error)
	// ListPagedDocuments returns a page of the Documents the caller can read,
	// newest first.
	ListPagedDocuments(ctx context.Context, page *types.Pagination) (*types.PageResult, error)
}

// DocumentRepository persists Documents and their Access Lists.
type DocumentRepository interface {
	// CreateDocument inserts doc and grants it to its owner and to every name
	// on accessList, all or nothing. An unknown name is ErrPrincipalNotFound
	// or ErrGroupNotFound; the owner already holding that content is
	// ErrDuplicateDocument.
	CreateDocument(ctx context.Context, doc *types.Document, accessList types.AccessList) error
	// GetDocumentByOwnerAndHash returns the owner's Document with that
	// content hash, or ErrDocumentNotFound.
	GetDocumentByOwnerAndHash(ctx context.Context, ownerID uuid.UUID, fileHash string) (*types.Document, error)
	// GetReadableDocumentByID returns the Document if principalID can read
	// it, else ErrDocumentNotFound.
	GetReadableDocumentByID(ctx context.Context, principalID, id uuid.UUID) (*types.Document, error)
	// ListPagedReadableDocuments returns a page of the Documents principalID
	// can read, newest first, and how many there are in all.
	ListPagedReadableDocuments(ctx context.Context, principalID uuid.UUID, page *types.Pagination) ([]*types.Document, int64, error)
	// GetAccessList returns the names on a Document's Access List.
	GetAccessList(ctx context.Context, id uuid.UUID) (*types.AccessList, error)
}
