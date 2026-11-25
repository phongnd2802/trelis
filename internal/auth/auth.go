// Package auth knows who is asking: Principals, the Groups they belong to,
// and the API keys they authenticate with.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/phongnd2802/trelis/internal/db/sqlc"
)

var (
	// ErrUnauthenticated means a key belongs to no Principal or has been revoked.
	ErrUnauthenticated = errors.New("auth: unauthenticated")
	// ErrNotFound means no Principal or Group has the given name, or no API
	// key has the given ID.
	ErrNotFound = errors.New("auth: not found")
	// ErrExists means a Principal or Group with that name already exists.
	ErrExists = errors.New("auth: already exists")
)

// Principal is the identity a request is made as.
type Principal struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Groups []Group   `json:"groups"`
}

// Group is a named set of Principals that access can be granted to at once.
type Group struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// Store keeps Principals, Groups and API keys in Postgres.
type Store struct {
	q *sqlc.Queries
}

// NewStore returns a Store on a migrated database.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{q: sqlc.New(pool)}
}

// CreatePrincipal adds a Principal with a unique name.
func (s *Store) CreatePrincipal(ctx context.Context, name string) (Principal, error) {
	row, err := s.q.CreatePrincipal(ctx, name)
	if err != nil {
		return Principal{}, createErr("principal", name, err)
	}
	return Principal{ID: row.ID, Name: row.Name, Groups: []Group{}}, nil
}

// CreateGroup adds a Group with a unique name.
func (s *Store) CreateGroup(ctx context.Context, name string) (Group, error) {
	row, err := s.q.CreateGroup(ctx, name)
	if err != nil {
		return Group{}, createErr("group", name, err)
	}
	return Group{ID: row.ID, Name: row.Name}, nil
}

// AddMember puts the named Principal in the named Group. Adding an existing
// member again is a no-op.
func (s *Store) AddMember(ctx context.Context, group, principal string) error {
	g, err := s.q.GetGroupByName(ctx, group)
	if err != nil {
		return lookupErr("group", group, err)
	}
	p, err := s.q.GetPrincipalByName(ctx, principal)
	if err != nil {
		return lookupErr("principal", principal, err)
	}
	if err := s.q.AddGroupMember(ctx, sqlc.AddGroupMemberParams{GroupID: g.ID, PrincipalID: p.ID}); err != nil {
		return fmt.Errorf("auth: add %q to %q: %w", principal, group, err)
	}
	return nil
}

// keyPrefix marks Trelis API keys so they are recognizable in config files
// and secret scanners.
const keyPrefix = "trelis_"

// CreateKey issues a new API key for the named Principal and returns it with
// its description. Only its hash is stored, so this is the only time the key
// can be read.
func (s *Store) CreateKey(ctx context.Context, principal string) (APIKey, string, error) {
	p, err := s.q.GetPrincipalByName(ctx, principal)
	if err != nil {
		return APIKey{}, "", lookupErr("principal", principal, err)
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // crypto/rand.Read never returns an error
	key := keyPrefix + base64.RawURLEncoding.EncodeToString(secret)
	row, err := s.q.CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{PrincipalID: p.ID, KeyHash: hashKey(key)})
	if err != nil {
		return APIKey{}, "", fmt.Errorf("auth: create key for %q: %w", principal, err)
	}
	return APIKey(row), key, nil
}

// APIKey describes one of a Principal's keys, never the key itself. Its
// fields match the sqlc rows of the key queries, so a row converts to it.
type APIKey struct {
	ID        uuid.UUID
	CreatedAt time.Time
	// RevokedAt is when the key was revoked, or nil while it still works.
	RevokedAt *time.Time
}

// ListKeys describes the named Principal's keys, oldest first, revoked ones
// included.
func (s *Store) ListKeys(ctx context.Context, principal string) ([]APIKey, error) {
	p, err := s.q.GetPrincipalByName(ctx, principal)
	if err != nil {
		return nil, lookupErr("principal", principal, err)
	}
	rows, err := s.q.ListAPIKeys(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("auth: keys of %q: %w", principal, err)
	}
	keys := make([]APIKey, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, APIKey(r))
	}
	return keys, nil
}

// RevokeKey stops the key with that ID from authenticating, from the next
// request on. Revoking a revoked key again is a no-op; an unknown ID is
// ErrNotFound.
func (s *Store) RevokeKey(ctx context.Context, id uuid.UUID) error {
	n, err := s.q.RevokeAPIKey(ctx, id)
	if err != nil {
		return fmt.Errorf("auth: revoke key %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: key %s", ErrNotFound, id)
	}
	return nil
}

// Authenticate returns the Principal that key belongs to, with their Groups,
// or ErrUnauthenticated if it belongs to none or has been revoked.
func (s *Store) Authenticate(ctx context.Context, key string) (Principal, error) {
	row, err := s.q.GetPrincipalByKeyHash(ctx, hashKey(key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: authenticate: %w", err)
	}
	rows, err := s.q.ListPrincipalGroups(ctx, row.ID)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: groups of %q: %w", row.Name, err)
	}
	groups := make([]Group, 0, len(rows))
	for _, g := range rows {
		groups = append(groups, Group{ID: g.ID, Name: g.Name})
	}
	return Principal{ID: row.ID, Name: row.Name, Groups: groups}, nil
}

// createErr wraps a failed insert, as ErrExists when the name is taken.
func createErr(kind, name string, err error) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: %s %q", ErrExists, kind, name)
	}
	return fmt.Errorf("auth: create %s %q: %w", kind, name, err)
}

// uniqueViolation is Postgres's SQLSTATE for a unique constraint violation.
const uniqueViolation = "23505"

// lookupErr wraps a failed by-name lookup, as ErrNotFound when nothing matched.
func lookupErr(kind, name string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s %q", ErrNotFound, kind, name)
	}
	return fmt.Errorf("auth: %s %q: %w", kind, name, err)
}

// hashKey is what is stored for a key. Keys carry 256 random bits, so a fast
// unsalted hash is enough: there is nothing to brute-force.
func hashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

type principalKey struct{}

// NewContext returns ctx carrying the authenticated Principal.
func NewContext(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the Principal a request was authenticated as.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
