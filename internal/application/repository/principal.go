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
	// ErrPrincipalNotFound means no Principal has the given name.
	ErrPrincipalNotFound = errors.New("principal not found")
	// ErrGroupNotFound means no Group has the given name.
	ErrGroupNotFound = errors.New("group not found")
	// ErrPrincipalExists means a Principal with that name already exists.
	ErrPrincipalExists = errors.New("principal already exists")
	// ErrGroupExists means a Group with that name already exists.
	ErrGroupExists = errors.New("group already exists")
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
	var grants types.AccessGrants
	for _, name := range list.Principals {
		p, err := r.GetPrincipalByName(ctx, name)
		if err != nil {
			return types.AccessGrants{}, err
		}
		grants.PrincipalIDs = append(grants.PrincipalIDs, p.ID)
	}
	for _, name := range list.Groups {
		g, err := r.GetGroupByName(ctx, name)
		if err != nil {
			return types.AccessGrants{}, err
		}
		grants.GroupIDs = append(grants.GroupIDs, g.ID)
	}
	return grants, nil
}

func (r *principalRepository) CreatePrincipal(ctx context.Context, name string) (types.Principal, error) {
	row, err := r.store.Querier(ctx).CreatePrincipal(ctx, name)
	if db.IsUniqueViolation(err) {
		return types.Principal{}, fmt.Errorf("%w: %s", ErrPrincipalExists, name)
	}
	if err != nil {
		return types.Principal{}, fmt.Errorf("create principal %q: %w", name, err)
	}
	return types.Principal{ID: row.ID, Name: row.Name, Groups: []types.Group{}}, nil
}

func (r *principalRepository) CreateGroup(ctx context.Context, name string) (types.Group, error) {
	row, err := r.store.Querier(ctx).CreateGroup(ctx, name)
	if db.IsUniqueViolation(err) {
		return types.Group{}, fmt.Errorf("%w: %s", ErrGroupExists, name)
	}
	if err != nil {
		return types.Group{}, fmt.Errorf("create group %q: %w", name, err)
	}
	return types.Group{ID: row.ID, Name: row.Name}, nil
}

func (r *principalRepository) GetPrincipalByName(ctx context.Context, name string) (types.Principal, error) {
	row, err := r.store.Querier(ctx).GetPrincipalByName(ctx, name)
	if errors.Is(err, db.ErrNotFound) {
		return types.Principal{}, fmt.Errorf("%w: %s", ErrPrincipalNotFound, name)
	}
	if err != nil {
		return types.Principal{}, fmt.Errorf("principal %q: %w", name, err)
	}
	return types.Principal{ID: row.ID, Name: row.Name}, nil
}

func (r *principalRepository) GetGroupByName(ctx context.Context, name string) (types.Group, error) {
	row, err := r.store.Querier(ctx).GetGroupByName(ctx, name)
	if errors.Is(err, db.ErrNotFound) {
		return types.Group{}, fmt.Errorf("%w: %s", ErrGroupNotFound, name)
	}
	if err != nil {
		return types.Group{}, fmt.Errorf("group %q: %w", name, err)
	}
	return types.Group{ID: row.ID, Name: row.Name}, nil
}

func (r *principalRepository) AddMember(ctx context.Context, groupID, principalID uuid.UUID) error {
	err := r.store.Querier(ctx).AddGroupMember(ctx, sqlc.AddGroupMemberParams{GroupID: groupID, PrincipalID: principalID})
	if err != nil {
		return fmt.Errorf("add principal %s to group %s: %w", principalID, groupID, err)
	}
	return nil
}

func (r *principalRepository) ListGroups(ctx context.Context, principalID uuid.UUID) ([]types.Group, error) {
	rows, err := r.store.Querier(ctx).ListPrincipalGroups(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("groups of principal %s: %w", principalID, err)
	}
	groups := make([]types.Group, 0, len(rows))
	for _, g := range rows {
		groups = append(groups, types.Group{ID: g.ID, Name: g.Name})
	}
	return groups, nil
}
