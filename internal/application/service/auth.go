package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
	"github.com/phongnd2802/trelis/internal/utils"
)

// authService implements AuthService over the API key and Principal
// repositories.
type authService struct {
	principals interfaces.PrincipalRepository
	keys       interfaces.APIKeyRepository
}

// NewAuthService returns the service the HTTP middleware authenticates with.
func NewAuthService(principals interfaces.PrincipalRepository, keys interfaces.APIKeyRepository) interfaces.AuthService {
	return &authService{principals: principals, keys: keys}
}

// Authenticate looks the key up by its hash, so a revoked key fails on the
// very next request.
func (s *authService) Authenticate(ctx context.Context, key string) (types.Principal, error) {
	p, err := s.keys.GetPrincipalByKeyHash(ctx, utils.HashAPIKey(key))
	if errors.Is(err, repository.ErrAPIKeyNotFound) {
		return types.Principal{}, interfaces.ErrUnauthenticated
	}
	if err != nil {
		return types.Principal{}, fmt.Errorf("authenticate: %w", err)
	}
	if p.Groups, err = s.principals.ListGroups(ctx, p.ID); err != nil {
		return types.Principal{}, fmt.Errorf("authenticate %q: %w", p.Name, err)
	}
	return p, nil
}
