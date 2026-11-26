package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

var (
	// ErrPrincipalNotFound means an Access List names no existing Principal.
	ErrPrincipalNotFound = errors.New("principal not found")
	// ErrGroupNotFound means an Access List names no existing Group.
	ErrGroupNotFound = errors.New("group not found")
)

type principalRepository struct {
	store db.Store
}

// NewPrincipalRepository returns a PrincipalRepository on store.
func NewPrincipalRepository(store db.Store) interfaces.PrincipalRepository {
	return &principalRepository{store: store}
}

// ResolveNames looks every name up in ctx's transaction, if any.
func (r *principalRepository) ResolveNames(ctx context.Context, list types.AccessList) (types.AccessGrants, error) {
	q := r.store.Querier(ctx)
	var grants types.AccessGrants
	for _, name := range list.Principals {
		p, err := q.GetPrincipalByName(ctx, name)
		if errors.Is(err, db.ErrNotFound) {
			return types.AccessGrants{}, fmt.Errorf("%w: %s", ErrPrincipalNotFound, name)
		}
		if err != nil {
			return types.AccessGrants{}, fmt.Errorf("principal %q: %w", name, err)
		}
		grants.PrincipalIDs = append(grants.PrincipalIDs, p.ID)
	}
	for _, name := range list.Groups {
		g, err := q.GetGroupByName(ctx, name)
		if errors.Is(err, db.ErrNotFound) {
			return types.AccessGrants{}, fmt.Errorf("%w: %s", ErrGroupNotFound, name)
		}
		if err != nil {
			return types.AccessGrants{}, fmt.Errorf("group %q: %w", name, err)
		}
		grants.GroupIDs = append(grants.GroupIDs, g.ID)
	}
	return grants, nil
}
