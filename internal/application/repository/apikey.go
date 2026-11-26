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

// ErrAPIKeyNotFound means no API key has the given ID, or no unrevoked key
// has the given hash.
var ErrAPIKeyNotFound = errors.New("api key not found")

type apiKeyRepository struct {
	store db.Store
}

// NewAPIKeyRepository returns an APIKeyRepository on store.
func NewAPIKeyRepository(store db.Store) interfaces.APIKeyRepository {
	return &apiKeyRepository{store: store}
}

func (r *apiKeyRepository) CreateAPIKey(ctx context.Context, principalID uuid.UUID, keyHash []byte) (types.APIKey, error) {
	row, err := r.store.Querier(ctx).CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{PrincipalID: principalID, KeyHash: keyHash})
	if err != nil {
		return types.APIKey{}, fmt.Errorf("create key for principal %s: %w", principalID, err)
	}
	return types.APIKey{ID: row.ID, CreatedAt: row.CreatedAt, RevokedAt: row.RevokedAt}, nil
}

func (r *apiKeyRepository) ListAPIKeys(ctx context.Context, principalID uuid.UUID) ([]types.APIKey, error) {
	rows, err := r.store.Querier(ctx).ListAPIKeys(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("keys of principal %s: %w", principalID, err)
	}
	keys := make([]types.APIKey, 0, len(rows))
	for _, k := range rows {
		keys = append(keys, types.APIKey{ID: k.ID, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt})
	}
	return keys, nil
}

func (r *apiKeyRepository) RevokeAPIKey(ctx context.Context, id uuid.UUID) error {
	n, err := r.store.Querier(ctx).RevokeAPIKey(ctx, id)
	if err != nil {
		return fmt.Errorf("revoke key %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrAPIKeyNotFound, id)
	}
	return nil
}

func (r *apiKeyRepository) GetPrincipalByKeyHash(ctx context.Context, keyHash []byte) (types.Principal, error) {
	row, err := r.store.Querier(ctx).GetPrincipalByKeyHash(ctx, keyHash)
	if errors.Is(err, db.ErrNotFound) {
		return types.Principal{}, ErrAPIKeyNotFound
	}
	if err != nil {
		return types.Principal{}, fmt.Errorf("principal by key: %w", err)
	}
	return types.Principal{ID: row.ID, Name: row.Name}, nil
}
