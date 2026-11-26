package interfaces

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// AuthService authenticates requests. It is all the HTTP middleware needs.
type AuthService interface {
	// Authenticate returns the Principal that key belongs to, with their
	// Groups, or ErrUnauthenticated if it belongs to no one or has been
	// revoked. Any other error means the key could not be checked.
	Authenticate(ctx context.Context, key string) (types.Principal, error)
}

// ErrUnauthenticated is returned by Authenticate for a key that belongs to
// no Principal or has been revoked.
var ErrUnauthenticated = errors.New("unauthenticated")

// PrincipalService is the operator's use cases on Principals, Groups and API
// keys, run by the admin CLI, and the caller's view of themselves for
// GET /me. Unknown and duplicate names are the PrincipalRepository's and
// APIKeyRepository's errors, wrapped.
type PrincipalService interface {
	// CreatePrincipal adds a Principal with a unique name.
	CreatePrincipal(ctx context.Context, name string) (types.Principal, error)
	// CreateGroup adds a Group with a unique name.
	CreateGroup(ctx context.Context, name string) (types.Group, error)
	// AddMember puts the named Principal in the named Group. Adding an
	// existing member again is a no-op.
	AddMember(ctx context.Context, group, principal string) error
	// CreateAPIKey issues a new key for the named Principal and returns its
	// description and the key. Only its hash is stored, so this is the only
	// time the key can be read.
	CreateAPIKey(ctx context.Context, principal string) (types.APIKey, string, error)
	// ListAPIKeys describes the named Principal's keys, oldest first,
	// revoked ones included.
	ListAPIKeys(ctx context.Context, principal string) ([]types.APIKey, error)
	// RevokeAPIKey stops the key with that ID from authenticating, from the
	// next request on. Revoking a revoked key again is a no-op.
	RevokeAPIKey(ctx context.Context, id uuid.UUID) error
	// GetCurrentPrincipal returns the Principal in ctx (types.WithPrincipal),
	// as it was authenticated.
	GetCurrentPrincipal(ctx context.Context) (types.Principal, error)
}

// PrincipalRepository persists Principals, Groups and Group membership
// (ADR-0007).
type PrincipalRepository interface {
	// ResolveNames turns the names on an Access List into IDs. An unknown
	// name is ErrPrincipalNotFound or ErrGroupNotFound, wrapped with the
	// name. It is the one place Access List names are resolved.
	ResolveNames(ctx context.Context, list types.AccessList) (types.AccessGrants, error)
	// CreatePrincipal inserts a Principal, with no Groups. A taken name is
	// ErrPrincipalExists.
	CreatePrincipal(ctx context.Context, name string) (types.Principal, error)
	// CreateGroup inserts a Group. A taken name is ErrGroupExists.
	CreateGroup(ctx context.Context, name string) (types.Group, error)
	// GetPrincipalByName returns the Principal, without their Groups, or
	// ErrPrincipalNotFound.
	GetPrincipalByName(ctx context.Context, name string) (types.Principal, error)
	// GetGroupByName returns the Group or ErrGroupNotFound.
	GetGroupByName(ctx context.Context, name string) (types.Group, error)
	// AddMember puts the Principal in the Group; doing it twice is a no-op.
	AddMember(ctx context.Context, groupID, principalID uuid.UUID) error
	// ListGroups returns the Groups a Principal belongs to, by name, as an
	// empty, non-nil list when there are none.
	ListGroups(ctx context.Context, principalID uuid.UUID) ([]types.Group, error)
}

// APIKeyRepository persists API keys by their hash (ADR-0007).
type APIKeyRepository interface {
	// CreateAPIKey stores a key hash for the Principal.
	CreateAPIKey(ctx context.Context, principalID uuid.UUID, keyHash []byte) (types.APIKey, error)
	// ListAPIKeys returns the Principal's keys, oldest first.
	ListAPIKeys(ctx context.Context, principalID uuid.UUID) ([]types.APIKey, error)
	// RevokeAPIKey revokes the key, keeping the first revocation time. An
	// unknown ID is ErrAPIKeyNotFound.
	RevokeAPIKey(ctx context.Context, id uuid.UUID) error
	// GetPrincipalByKeyHash returns the Principal, without their Groups,
	// holding an unrevoked key with that hash, or ErrAPIKeyNotFound.
	GetPrincipalByKeyHash(ctx context.Context, keyHash []byte) (types.Principal, error)
}
