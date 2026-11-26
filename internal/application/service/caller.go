package service

import (
	"context"
	"errors"

	"github.com/phongnd2802/trelis/internal/types"
)

// caller is the Principal a request was authenticated as. Every route behind
// the router's authenticate middleware has one.
func caller(ctx context.Context) (types.Principal, error) {
	p, ok := types.PrincipalFromContext(ctx)
	if !ok {
		return types.Principal{}, errors.New("no authenticated principal in context")
	}
	return p, nil
}
