// Package repository persists Trelis's domain types in Postgres through the
// sqlc queries in internal/db/queries.
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/sqlc"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

var (
	// ErrDocumentNotFound means no Document matched, or the Principal asking
	// cannot read the one that did.
	ErrDocumentNotFound = errors.New("document not found")
	// ErrDuplicateDocument means the owner already has a Document with that
	// content hash.
	ErrDuplicateDocument = errors.New("document with the same content already exists")
)

type documentRepository struct {
	store db.Store
}

// NewDocumentRepository returns a DocumentRepository on store.
func NewDocumentRepository(store db.Store) interfaces.DocumentRepository {
	return &documentRepository{store: store}
}

// CreateDocument inserts doc and grants it to its owner and to grants. It
// starts no transaction of its own: call it inside the service's ExecTx so
// a failed grant leaves no Document behind.
func (r *documentRepository) CreateDocument(ctx context.Context, doc *types.Document, grants types.AccessGrants) error {
	q := r.store.Querier(ctx)
	row, err := q.CreateDocument(ctx, sqlc.CreateDocumentParams{
		ID:        doc.ID,
		OwnerID:   doc.OwnerID,
		Title:     doc.Title,
		FileName:  doc.FileName,
		FileType:  doc.FileType,
		FileSize:  doc.FileSize,
		FileHash:  doc.FileHash,
		UploadID:  doc.UploadID,
		ObjectKey: doc.ObjectKey,
	})
	if db.IsUniqueViolation(err) {
		return ErrDuplicateDocument
	}
	if err != nil {
		return fmt.Errorf("create document %q: %w", doc.FileName, err)
	}
	// The owner is always on the Access List; granting twice is a no-op.
	for _, p := range append([]uuid.UUID{doc.OwnerID}, grants.PrincipalIDs...) {
		if err := q.GrantDocumentToPrincipal(ctx, sqlc.GrantDocumentToPrincipalParams{DocumentID: doc.ID, PrincipalID: p}); err != nil {
			return fmt.Errorf("grant document to principal %s: %w", p, err)
		}
	}
	for _, g := range grants.GroupIDs {
		if err := q.GrantDocumentToGroup(ctx, sqlc.GrantDocumentToGroupParams{DocumentID: doc.ID, GroupID: g}); err != nil {
			return fmt.Errorf("grant document to group %s: %w", g, err)
		}
	}
	*doc = *fromRow(row)
	return nil
}

// GetDocumentByOwnerAndHash finds the owner's upload of the same content.
func (r *documentRepository) GetDocumentByOwnerAndHash(ctx context.Context, ownerID uuid.UUID, fileHash string) (*types.Document, error) {
	row, err := r.store.Querier(ctx).GetDocumentByOwnerAndHash(ctx, sqlc.GetDocumentByOwnerAndHashParams{OwnerID: ownerID, FileHash: fileHash})
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document by hash: %w", err)
	}
	return fromRow(row), nil
}

// GetDocumentByID returns the Document without checking an Access List. It is
// the system view the ingest activities use, which run with no Principal.
func (r *documentRepository) GetDocumentByID(ctx context.Context, id uuid.UUID) (*types.Document, error) {
	row, err := r.store.Querier(ctx).GetDocumentByID(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document %s: %w", id, err)
	}
	return fromRow(row), nil
}

// SetParseStatus sets a Document's parse status and, when it failed, the
// reason it is shown with; any other status clears the reason. A missing row
// is ErrDocumentNotFound, so a caller can tell a deleted Document from a lost
// update.
func (r *documentRepository) SetParseStatus(ctx context.Context, id uuid.UUID, status, parseError string) error {
	rows, err := r.store.Querier(ctx).SetDocumentParseStatus(ctx, sqlc.SetDocumentParseStatusParams{
		ID:          id,
		ParseStatus: status,
		ParseError:  parseError,
	})
	if err != nil {
		return fmt.Errorf("set parse status of document %s: %w", id, err)
	}
	if rows == 0 {
		return ErrDocumentNotFound
	}
	return nil
}

// GetReadableDocumentByID returns the Document if principalID can read it.
func (r *documentRepository) GetReadableDocumentByID(ctx context.Context, principalID, id uuid.UUID) (*types.Document, error) {
	row, err := r.store.Querier(ctx).GetReadableDocument(ctx, sqlc.GetReadableDocumentParams{ID: id, PrincipalID: principalID})
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document %s: %w", id, err)
	}
	return fromRow(row), nil
}

// ListPagedReadableDocuments returns one page and the total count.
func (r *documentRepository) ListPagedReadableDocuments(ctx context.Context, principalID uuid.UUID, page *types.Pagination) ([]*types.Document, int64, error) {
	q := r.store.Querier(ctx)
	total, err := q.CountReadableDocuments(ctx, principalID)
	if err != nil {
		return nil, 0, fmt.Errorf("count documents: %w", err)
	}
	rows, err := q.ListReadableDocuments(ctx, sqlc.ListReadableDocumentsParams{
		PrincipalID: principalID,
		PageOffset:  int32(page.Offset()),
		PageLimit:   int32(page.Limit()),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list documents: %w", err)
	}
	docs := make([]*types.Document, 0, len(rows))
	for _, row := range rows {
		docs = append(docs, fromRow(row))
	}
	return docs, total, nil
}

// GetAccessList returns the names on a Document's Access List, sorted.
func (r *documentRepository) GetAccessList(ctx context.Context, id uuid.UUID) (*types.AccessList, error) {
	q := r.store.Querier(ctx)
	principals, err := q.ListDocumentPrincipals(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("access list of %s: %w", id, err)
	}
	groups, err := q.ListDocumentGroups(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("access list of %s: %w", id, err)
	}
	return &types.AccessList{Principals: nonNil(principals), Groups: nonNil(groups)}, nil
}

// nonNil makes an empty list encode as [] rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func fromRow(r sqlc.Document) *types.Document {
	doc := &types.Document{
		ID:          r.ID,
		OwnerID:     r.OwnerID,
		Title:       r.Title,
		FileName:    r.FileName,
		FileType:    r.FileType,
		FileSize:    r.FileSize,
		FileHash:    r.FileHash,
		UploadID:    r.UploadID,
		ObjectKey:   r.ObjectKey,
		ParseStatus: r.ParseStatus,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
	if r.ParseError.Valid {
		doc.ParseError = r.ParseError.String
	}
	return doc
}
