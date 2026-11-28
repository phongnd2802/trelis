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
	// EnsureCanReplace checks that the caller can read the Document and owns
	// it, with the same not-found and forbidden errors ReplaceDocumentFile
	// answers. A handler can call it before reading an upload it would have
	// to reject.
	EnsureCanReplace(ctx context.Context, id uuid.UUID) error
	// ReplaceDocumentFile replaces a Document's Source File with file, under
	// a new upload ID, keeping its Access List. Only the owner may replace
	// it: a caller who can read it but is not its owner gets a forbidden
	// error, and one who cannot read it gets the same not-found error as for
	// a missing Document. The same content as the Document's current upload
	// or another Document of the owner's is a duplicate error carrying that
	// Document. The Document is pending again and its old Chunks are gone
	// before this returns.
	ReplaceDocumentFile(ctx context.Context, id uuid.UUID, file *multipart.FileHeader) (*types.Document, error)
	// GetDocumentByID returns a Document the caller can read; one that does
	// not exist and one they cannot read are the same not-found error.
	GetDocumentByID(ctx context.Context, id uuid.UUID) (*types.Document, error)
	// GetDocumentFile opens the Source File of a Document the caller can
	// read, with the same not-found error as GetDocumentByID. The caller
	// closes the reader.
	GetDocumentFile(ctx context.Context, id uuid.UUID) (*types.Document, io.ReadCloser, error)
	// DeleteDocument removes a Document the caller owns and everything derived
	// from it. Only the owner may delete it: a caller who can read it but is
	// not its owner gets a forbidden error, and one who cannot read it gets
	// the same not-found error as for a missing Document. It starts the
	// workflow that removes the Document's objects.
	DeleteDocument(ctx context.Context, id uuid.UUID) error
	// ListPagedDocuments returns a page of the Documents the caller can read,
	// newest first.
	ListPagedDocuments(ctx context.Context, page *types.Pagination) (*types.PageResult, error)
	// IngestEventsAfter returns the Ingest Events of a Document the caller
	// can read whose Seq is greater than afterSeq, in order, and the
	// Document's current upload ID. It checks the Access List on every call,
	// so a revoked grant or a deleted Document ends an open event stream on
	// its next poll; a caller who cannot read the Document gets the same
	// not-found error as for a missing one.
	IngestEventsAfter(ctx context.Context, id uuid.UUID, afterSeq int64) ([]*types.IngestEvent, uuid.UUID, error)
}

// DocumentCleanupService is the use case the delete workflow's activity
// calls once a Document's row is gone: removing the objects under its prefix
// (ADR-0006).
type DocumentCleanupService interface {
	// DeleteDocumentObjects removes every object under the Document's
	// documents/{document_id}/ prefix. An empty prefix is not an error, so a
	// retried activity or a second cleanup run is harmless.
	DeleteDocumentObjects(ctx context.Context, documentID uuid.UUID) error
}

// DocumentUseCases is the whole Document service: the handler's
// DocumentService and the delete workflow activity's DocumentCleanupService.
// The constructor returns one value that wires both.
type DocumentUseCases interface {
	DocumentService
	DocumentCleanupService
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
	// ReplaceDocumentUpload applies updated's new upload and file metadata in
	// place, but only while the Document's upload ID is still
	// previousUploadID. It starts no transaction: the caller runs it in one
	// with the Chunk delete and the upload_replaced event. It returns
	// ErrUploadChanged when a concurrent re-upload won, ErrDocumentNotFound
	// when the row is gone and ErrDuplicateDocument when the owner already
	// has another Document with that content.
	ReplaceDocumentUpload(ctx context.Context, updated *types.Document, previousUploadID uuid.UUID) (*types.Document, error)
	// GetDocumentByID returns the Document without checking any Access List.
	// It is the system view the ingest activities use, which run with no
	// Principal; it returns ErrDocumentNotFound when no row matches.
	GetDocumentByID(ctx context.Context, id uuid.UUID) (*types.Document, error)
	// GetDocumentUploadID returns the Document's current upload ID without
	// locking. An ingest activity calls it after its Ingest Event insert,
	// which already holds the Document row lock, to notice that the upload it
	// works for was replaced.
	GetDocumentUploadID(ctx context.Context, id uuid.UUID) (uuid.UUID, error)
	// LockDocumentUploadID returns the Document's current upload ID, locking
	// the row until the surrounding transaction ends. Before deleting a
	// replaced upload's objects, the caller checks that the Document still
	// points at the upload it parsed.
	LockDocumentUploadID(ctx context.Context, id uuid.UUID) (uuid.UUID, error)
	// SetParseStatus sets a Document's parse status and, when it is failed, the
	// reason shown with it; any other status clears the reason. It returns
	// ErrDocumentNotFound when no row matches.
	SetParseStatus(ctx context.Context, id uuid.UUID, status, parseError string) error
	// DeleteDocument removes the Document row and everything derived from it,
	// returning the upload ID the row had when it was deleted so the cleanup
	// cancels that upload's ingest. It returns ErrDocumentNotFound when no row
	// matched, which covers a concurrent delete.
	DeleteDocument(ctx context.Context, id uuid.UUID) (uuid.UUID, error)
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
	// ListPendingDocuments returns the current upload of every Document with
	// parse_status pending, ordered by Document ID. That includes a Document
	// whose workflow is queued but has not set processing yet; starting it
	// again joins the open run because the workflow ID carries the Document
	// and upload IDs.
	ListPendingDocuments(ctx context.Context) ([]types.PendingDocument, error)
	// ListDocumentUploads returns the current upload and parse status of each
	// Document in ids that still exists, keyed by Document ID.
	ListDocumentUploads(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.DocumentUpload, error)
}
