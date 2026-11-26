// Package documents stores uploaded Documents with their Access Lists and
// answers which Documents a Principal can read.
//
// The uploaded bytes are kept in Postgres next to the Document (ADR-0002),
// so ingest needs only the Document ID to find them (ADR-0005).
package documents

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/phongnd2802/trelis/internal/db/sqlc"
)

var (
	// ErrNotFound means no Document has that ID or the caller cannot read
	// it; the two are deliberately the same, so existence is not revealed.
	ErrNotFound = errors.New("documents: not found")
	// ErrUnknownReader means an Access List names a Principal or Group that
	// does not exist.
	ErrUnknownReader = errors.New("documents: unknown principal or group")
	// ErrInvalid means an upload is missing its filename or content.
	ErrInvalid = errors.New("documents: invalid upload")
)

// StatusPending is a Document's status from upload until ingest finishes.
const StatusPending = "pending"

// Document describes one uploaded file, never its content.
type Document struct {
	ID          uuid.UUID `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	// AccessList is set by Get and Upload; List leaves it out.
	AccessList *AccessList `json:"access_list,omitempty"`
}

// AccessList names the Principals and Groups that may read a Document.
type AccessList struct {
	Principals []string `json:"principals"`
	Groups     []string `json:"groups"`
}

// Upload is a file to store and who may read it.
type Upload struct {
	Filename    string
	ContentType string
	Content     []byte
	// Readers is the Access List. The uploader is always added to it.
	Readers AccessList
}

// Store keeps Documents in Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store on a migrated database.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Upload stores the file as a pending Document owned by owner, readable by
// owner and by everyone on u.Readers. Nothing is stored if any name on the
// Access List is unknown (ErrUnknownReader).
func (s *Store) Upload(ctx context.Context, owner uuid.UUID, u Upload) (Document, error) {
	if u.Filename == "" || len(u.Content) == 0 {
		return Document{}, fmt.Errorf("%w: a file needs a name and content", ErrInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Document{}, fmt.Errorf("documents: upload: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlc.New(tx)

	sum := sha256.Sum256(u.Content)
	row, err := q.CreateDocument(ctx, sqlc.CreateDocumentParams{
		OwnerID:     owner,
		Filename:    u.Filename,
		ContentType: u.ContentType,
		SizeBytes:   int64(len(u.Content)),
		Sha256:      sum[:],
	})
	if err != nil {
		return Document{}, fmt.Errorf("documents: upload %q: %w", u.Filename, err)
	}
	if err := q.CreateDocumentFile(ctx, sqlc.CreateDocumentFileParams{DocumentID: row.ID, Content: u.Content}); err != nil {
		return Document{}, fmt.Errorf("documents: store %q: %w", u.Filename, err)
	}
	if err := grant(ctx, q, row.ID, owner, u.Readers); err != nil {
		return Document{}, err
	}
	doc := fromRow(sqlc.GetReadableDocumentRow(row))
	if doc.AccessList, err = accessList(ctx, q, row.ID); err != nil {
		return Document{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Document{}, fmt.Errorf("documents: upload %q: %w", u.Filename, err)
	}
	return doc, nil
}

// grant puts owner and everyone on readers on the Document's Access List.
func grant(ctx context.Context, q *sqlc.Queries, doc, owner uuid.UUID, readers AccessList) error {
	if err := q.GrantDocumentToPrincipal(ctx, sqlc.GrantDocumentToPrincipalParams{DocumentID: doc, PrincipalID: owner}); err != nil {
		return fmt.Errorf("documents: grant owner: %w", err)
	}
	for _, name := range readers.Principals {
		p, err := q.GetPrincipalByName(ctx, name)
		if err != nil {
			return readerErr("principal", name, err)
		}
		if err := q.GrantDocumentToPrincipal(ctx, sqlc.GrantDocumentToPrincipalParams{DocumentID: doc, PrincipalID: p.ID}); err != nil {
			return fmt.Errorf("documents: grant principal %q: %w", name, err)
		}
	}
	for _, name := range readers.Groups {
		g, err := q.GetGroupByName(ctx, name)
		if err != nil {
			return readerErr("group", name, err)
		}
		if err := q.GrantDocumentToGroup(ctx, sqlc.GrantDocumentToGroupParams{DocumentID: doc, GroupID: g.ID}); err != nil {
			return fmt.Errorf("documents: grant group %q: %w", name, err)
		}
	}
	return nil
}

func readerErr(kind, name string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s %q", ErrUnknownReader, kind, name)
	}
	return fmt.Errorf("documents: %s %q: %w", kind, name, err)
}

// List returns the Documents principal can read, newest first.
func (s *Store) List(ctx context.Context, principal uuid.UUID) ([]Document, error) {
	rows, err := sqlc.New(s.pool).ListReadableDocuments(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("documents: list: %w", err)
	}
	docs := make([]Document, 0, len(rows))
	for _, r := range rows {
		docs = append(docs, fromRow(sqlc.GetReadableDocumentRow(r)))
	}
	return docs, nil
}

// Get returns the Document with that ID and its Access List, or ErrNotFound
// if there is none or principal cannot read it.
func (s *Store) Get(ctx context.Context, principal, id uuid.UUID) (Document, error) {
	q := sqlc.New(s.pool)
	row, err := q.GetReadableDocument(ctx, sqlc.GetReadableDocumentParams{ID: id, PrincipalID: principal})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Document{}, fmt.Errorf("documents: get %s: %w", id, err)
	}
	doc := fromRow(row)
	if doc.AccessList, err = accessList(ctx, q, id); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func accessList(ctx context.Context, q *sqlc.Queries, doc uuid.UUID) (*AccessList, error) {
	principals, err := q.ListDocumentPrincipals(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("documents: access list of %s: %w", doc, err)
	}
	groups, err := q.ListDocumentGroups(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("documents: access list of %s: %w", doc, err)
	}
	if groups == nil {
		groups = []string{}
	}
	return &AccessList{Principals: principals, Groups: groups}, nil
}

// fromRow converts any of the document queries' rows, which share one shape.
func fromRow(r sqlc.GetReadableDocumentRow) Document {
	return Document{
		ID:          r.ID,
		Filename:    r.Filename,
		ContentType: r.ContentType,
		SizeBytes:   r.SizeBytes,
		Status:      r.Status,
		CreatedAt:   r.CreatedAt,
	}
}
