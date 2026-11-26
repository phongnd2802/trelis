package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
	"github.com/phongnd2802/trelis/internal/utils"
)

// principalService implements PrincipalService over the Principal and API
// key repositories. Use cases that look a name up and then write run in one
// transaction (ADR-0007).
type principalService struct {
	store      db.Store
	principals interfaces.PrincipalRepository
	keys       interfaces.APIKeyRepository
}

// NewPrincipalService returns the admin use cases and GET /me.
func NewPrincipalService(store db.Store, principals interfaces.PrincipalRepository,
	keys interfaces.APIKeyRepository,
) interfaces.PrincipalService {
	return &principalService{store: store, principals: principals, keys: keys}
}

func (s *principalService) CreatePrincipal(ctx context.Context, name string) (types.Principal, error) {
	return s.principals.CreatePrincipal(ctx, name)
}

func (s *principalService) CreateGroup(ctx context.Context, name string) (types.Group, error) {
	return s.principals.CreateGroup(ctx, name)
}

func (s *principalService) AddMember(ctx context.Context, group, principal string) error {
	return s.store.ExecTx(ctx, func(ctx context.Context) error {
		g, err := s.principals.GetGroupByName(ctx, group)
		if err != nil {
			return err
		}
		p, err := s.principals.GetPrincipalByName(ctx, principal)
		if err != nil {
			return err
		}
		return s.principals.AddMember(ctx, g.ID, p.ID)
	})
}

func (s *principalService) CreateAPIKey(ctx context.Context, principal string) (types.APIKey, string, error) {
	key := utils.GenerateAPIKey()
	var issued types.APIKey
	err := s.store.ExecTx(ctx, func(ctx context.Context) error {
		p, err := s.principals.GetPrincipalByName(ctx, principal)
		if err != nil {
			return err
		}
		issued, err = s.keys.CreateAPIKey(ctx, p.ID, utils.HashAPIKey(key))
		return err
	})
	if err != nil {
		return types.APIKey{}, "", err
	}
	return issued, key, nil
}

func (s *principalService) ListAPIKeys(ctx context.Context, principal string) ([]types.APIKey, error) {
	p, err := s.principals.GetPrincipalByName(ctx, principal)
	if err != nil {
		return nil, err
	}
	return s.keys.ListAPIKeys(ctx, p.ID)
}

func (s *principalService) RevokeAPIKey(ctx context.Context, id uuid.UUID) error {
	return s.keys.RevokeAPIKey(ctx, id)
}

func (s *principalService) GetCurrentPrincipal(ctx context.Context) (types.Principal, error) {
	return caller(ctx)
}
