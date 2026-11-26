// Package repository persists Trelis's domain types in Postgres through the
// sqlc queries in internal/db/queries.
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

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
	// ErrPrincipalNotFound means an Access List names no existing Principal.
	ErrPrincipalNotFound = errors.New("principal not found")
	// ErrGroupNotFound means an Access List names no existing Group.
	ErrGroupNotFound = errors.New("group not found")
)

// uniqueViolation is Postgres's SQLSTATE for a unique constraint violation.
const uniqueViolation = "23505"

type documentRepository struct {
	pool *pgxpool.Pool
}

// NewDocumentRepository returns a DocumentRepository on a migrated database.
func NewDocumentRepository(pool *pgxpool.Pool) interfaces.DocumentRepository {
	return &documentRepository{pool: pool}
}

// CreateDocument inserts doc and its Access List in one transaction.
func (r *documentRepository) CreateDocument(ctx context.Context, doc *types.Document, accessList types.AccessList) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("create document: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlc.New(tx)

	row, err := q.CreateDocument(ctx, sqlc.CreateDocumentParams{
		ID:       doc.ID,
		OwnerID:  doc.OwnerID,
		Title:    doc.Title,
		FileName: doc.FileName,
		FileType: doc.FileType,
		FileSize: doc.FileSize,
		FileHash: doc.FileHash,
		FilePath: doc.FilePath,
	})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return ErrDuplicateDocument
	}
	if err != nil {
		return fmt.Errorf("create document %q: %w", doc.FileName, err)
	}
	if err := grant(ctx, q, doc.ID, doc.OwnerID, accessList); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("create document %q: %w", doc.FileName, err)
	}
	*doc = *fromRow(row)
	return nil
}

// grant puts owner and everyone on list on the Document's Access List.
func grant(ctx context.Context, q *sqlc.Queries, doc, owner uuid.UUID, list types.AccessList) error {
	if err := q.GrantDocumentToPrincipal(ctx, sqlc.GrantDocumentToPrincipalParams{DocumentID: doc, PrincipalID: owner}); err != nil {
		return fmt.Errorf("grant document to owner: %w", err)
	}
	for _, name := range list.Principals {
		p, err := q.GetPrincipalByName(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrPrincipalNotFound, name)
		}
		if err != nil {
			return fmt.Errorf("principal %q: %w", name, err)
		}
		if err := q.GrantDocumentToPrincipal(ctx, sqlc.GrantDocumentToPrincipalParams{DocumentID: doc, PrincipalID: p.ID}); err != nil {
			return fmt.Errorf("grant document to principal %q: %w", name, err)
		}
	}
	for _, name := range list.Groups {
		g, err := q.GetGroupByName(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrGroupNotFound, name)
		}
		if err != nil {
			return fmt.Errorf("group %q: %w", name, err)
		}
		if err := q.GrantDocumentToGroup(ctx, sqlc.GrantDocumentToGroupParams{DocumentID: doc, GroupID: g.ID}); err != nil {
			return fmt.Errorf("grant document to group %q: %w", name, err)
		}
	}
	return nil
}

// GetDocumentByOwnerAndHash finds the owner's upload of the same content.
func (r *documentRepository) GetDocumentByOwnerAndHash(ctx context.Context, ownerID uuid.UUID, fileHash string) (*types.Document, error) {
	row, err := sqlc.New(r.pool).GetDocumentByOwnerAndHash(ctx, sqlc.GetDocumentByOwnerAndHashParams{OwnerID: ownerID, FileHash: fileHash})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document by hash: %w", err)
	}
	return fromRow(row), nil
}

// GetReadableDocumentByID returns the Document if principalID can read it.
func (r *documentRepository) GetReadableDocumentByID(ctx context.Context, principalID, id uuid.UUID) (*types.Document, error) {
	row, err := sqlc.New(r.pool).GetReadableDocument(ctx, sqlc.GetReadableDocumentParams{ID: id, PrincipalID: principalID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document %s: %w", id, err)
	}
	return fromRow(row), nil
}

// ListPagedReadableDocuments returns one page and the total count.
func (r *documentRepository) ListPagedReadableDocuments(ctx context.Context, principalID uuid.UUID, page *types.Pagination) ([]*types.Document, int64, error) {
	q := sqlc.New(r.pool)
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
	q := sqlc.New(r.pool)
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
	return &types.Document{
		ID:          r.ID,
		OwnerID:     r.OwnerID,
		Title:       r.Title,
		FileName:    r.FileName,
		FileType:    r.FileType,
		FileSize:    r.FileSize,
		FileHash:    r.FileHash,
		FilePath:    r.FilePath,
		ParseStatus: r.ParseStatus,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}
