package interfaces

import (
	"context"
	"io"
	"mime/multipart"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// DocumentService is the Document use cases, always on behalf of the
// Principal in ctx (types.WithPrincipal). It answers with *errors.AppError for
// anything the client should see, and *types.DuplicateDocumentError when the
// caller already uploaded the same content.
type DocumentService interface {
	// CreateDocumentFromFile stores the file as a pending Document owned by
	// the caller, readable by the caller and everyone on accessList, and
	// starts its ingest workflow. A workflow that will not start does not
	// fail the upload.
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
	// CreateDocument inserts doc and grants it to its owner and to grants.
	// It starts no transaction: the caller runs it in one so a failed grant
	// leaves nothing. The owner already holding that content is
	// ErrDuplicateDocument.
	CreateDocument(ctx context.Context, doc *types.Document, grants types.AccessGrants) error
	// GetDocumentByOwnerAndHash returns the owner's Document with that
	// content hash, or ErrDocumentNotFound.
	GetDocumentByOwnerAndHash(ctx context.Context, ownerID uuid.UUID, fileHash string) (*types.Document, error)
	// GetDocumentByID returns the Document without checking any Access List.
	// It is the system view the ingest activities use, which run with no
	// Principal; it returns ErrDocumentNotFound when no row matches.
	GetDocumentByID(ctx context.Context, id uuid.UUID) (*types.Document, error)
	// SetParseStatus sets a Document's parse status and, when it is failed, the
	// reason shown with it; any other status clears the reason. It returns
	// ErrDocumentNotFound when no row matches.
	SetParseStatus(ctx context.Context, id uuid.UUID, status, parseError string) error
	// GetReadableDocumentByID returns the Document if principalID can read
	// it, else ErrDocumentNotFound.
	GetReadableDocumentByID(ctx context.Context, principalID, id uuid.UUID) (*types.Document, error)
	// ListPagedReadableDocuments returns a page of the Documents principalID
	// can read, newest first, and how many there are in all.
	ListPagedReadableDocuments(ctx context.Context, principalID uuid.UUID, page *types.Pagination) ([]*types.Document, int64, error)
	// ListReadableDocumentIDs returns the IDs of every Document principalID
	// can read that has finished ingest. Retrieval passes the set into every
	// search, so a Document mid-ingest (with partial embeddings) is never
	// searched.
	ListReadableDocumentIDs(ctx context.Context, principalID uuid.UUID) ([]uuid.UUID, error)
	// GetAccessList returns the names on a Document's Access List.
	GetAccessList(ctx context.Context, id uuid.UUID) (*types.AccessList, error)
	// ListPendingDocumentIDs returns the IDs of Documents with parse_status
	// pending, ordered by ID. That includes a Document whose workflow is
	// queued but has not set processing yet; starting it again joins the open
	// run because the workflow ID is the Document ID.
	ListPendingDocumentIDs(ctx context.Context) ([]uuid.UUID, error)
	// ListDocumentUploadIDs returns the current upload ID of each Document in
	// ids that still exists, keyed by Document ID.
	ListDocumentUploadIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
}
