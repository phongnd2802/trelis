package interfaces

import (
	"context"

	"github.com/phongnd2802/trelis/internal/types"
)

// PrincipalRepository persists Principals and Groups (ADR-0007).
type PrincipalRepository interface {
	// ResolveNames turns the names on an Access List into IDs. An unknown
	// name is ErrPrincipalNotFound or ErrGroupNotFound, wrapped with the
	// name. It is the one place Access List names are resolved.
	ResolveNames(ctx context.Context, list types.AccessList) (types.AccessGrants, error)
}
