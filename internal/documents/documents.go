// Package documents stores uploaded Documents with their Access Lists and
// answers which Documents a Principal can read.
//
// The uploaded bytes are kept in Postgres next to the Document (ADR-0002),
// so ingest needs only the Document ID to find them (ADR-0005). Whether a
// Principal can read a Document is decided in one place, the
// can_read_document SQL function.
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
	// ErrEmpty means an upload has no filename or no content.
	ErrEmpty = errors.New("documents: the file has no name or no content")
)

// UnknownNameError means an Access List names a Principal or Group that does
// not exist. Its message is fit to show the uploader.
type UnknownNameError struct {
	Kind string // "principal" or "group"
	Name string
}

func (e *UnknownNameError) Error() string {
	return fmt.Sprintf("unknown %s %q", e.Kind, e.Name)
}

// Document describes one uploaded file, never its content.
type Document struct {
	ID          uuid.UUID `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	// Status is "pending" from upload until ingest finishes.
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	// AccessList is shown only to the Document's owner, by Upload and Get.
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
	// AccessList is who may read it. The uploader is always added to it.
	AccessList AccessList
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
// owner and by everyone on u.AccessList. Nothing is stored if a name on the
// Access List is unknown (*UnknownNameError).
func (s *Store) Upload(ctx context.Context, owner uuid.UUID, u Upload) (Document, error) {
	if u.Filename == "" || len(u.Content) == 0 {
		return Document{}, ErrEmpty
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
	if err := grant(ctx, q, row.ID, owner, u.AccessList); err != nil {
		return Document{}, err
	}
	doc, err := withAccessList(ctx, q, row, owner)
	if err != nil {
		return Document{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Document{}, fmt.Errorf("documents: upload %q: %w", u.Filename, err)
	}
	return doc, nil
}

// grant puts owner and everyone on list on the Document's Access List.
func grant(ctx context.Context, q *sqlc.Queries, doc, owner uuid.UUID, list AccessList) error {
	if err := q.GrantDocumentToPrincipal(ctx, sqlc.GrantDocumentToPrincipalParams{DocumentID: doc, PrincipalID: owner}); err != nil {
		return fmt.Errorf("documents: grant owner: %w", err)
	}
	for _, name := range list.Principals {
		p, err := q.GetPrincipalByName(ctx, name)
		if err != nil {
			return lookupErr("principal", name, err)
		}
		if err := q.GrantDocumentToPrincipal(ctx, sqlc.GrantDocumentToPrincipalParams{DocumentID: doc, PrincipalID: p.ID}); err != nil {
			return fmt.Errorf("documents: grant principal %q: %w", name, err)
		}
	}
	for _, name := range list.Groups {
		g, err := q.GetGroupByName(ctx, name)
		if err != nil {
			return lookupErr("group", name, err)
		}
		if err := q.GrantDocumentToGroup(ctx, sqlc.GrantDocumentToGroupParams{DocumentID: doc, GroupID: g.ID}); err != nil {
			return fmt.Errorf("documents: grant group %q: %w", name, err)
		}
	}
	return nil
}

func lookupErr(kind, name string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return &UnknownNameError{Kind: kind, Name: name}
	}
	return fmt.Errorf("documents: %s %q: %w", kind, name, err)
}

// List returns the Documents principal can read, newest first, without
// their Access Lists.
func (s *Store) List(ctx context.Context, principal uuid.UUID) ([]Document, error) {
	rows, err := sqlc.New(s.pool).ListReadableDocuments(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("documents: list: %w", err)
	}
	docs := make([]Document, 0, len(rows))
	for _, r := range rows {
		docs = append(docs, fromRow(r))
	}
	return docs, nil
}

// Get returns the Document with that ID, or ErrNotFound if there is none or
// principal cannot read it. Its Access List is included for its owner.
func (s *Store) Get(ctx context.Context, principal, id uuid.UUID) (Document, error) {
	q := sqlc.New(s.pool)
	row, err := q.GetReadableDocument(ctx, sqlc.GetReadableDocumentParams{ID: id, PrincipalID: principal})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Document{}, fmt.Errorf("documents: get %s: %w", id, err)
	}
	return withAccessList(ctx, q, row, principal)
}

// withAccessList converts row, adding its Access List when caller owns it.
// Other readers do not learn who else can read the Document.
func withAccessList(ctx context.Context, q *sqlc.Queries, row sqlc.Document, caller uuid.UUID) (Document, error) {
	doc := fromRow(row)
	if row.OwnerID != caller {
		return doc, nil
	}
	principals, err := q.ListDocumentPrincipals(ctx, row.ID)
	if err != nil {
		return Document{}, fmt.Errorf("documents: access list of %s: %w", row.ID, err)
	}
	groups, err := q.ListDocumentGroups(ctx, row.ID)
	if err != nil {
		return Document{}, fmt.Errorf("documents: access list of %s: %w", row.ID, err)
	}
	doc.AccessList = &AccessList{Principals: nonNil(principals), Groups: nonNil(groups)}
	return doc, nil
}

// nonNil makes an empty list encode as [] rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func fromRow(r sqlc.Document) Document {
	return Document{
		ID:          r.ID,
		Filename:    r.Filename,
		ContentType: r.ContentType,
		SizeBytes:   r.SizeBytes,
		Status:      r.Status,
		CreatedAt:   r.CreatedAt,
	}
}
