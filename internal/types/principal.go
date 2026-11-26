package types

import (
	"context"
	"time"

	"github.com/google/uuid"
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

// APIKey describes one of a Principal's keys, never the key itself: only its
// hash is stored.
type APIKey struct {
	ID        uuid.UUID
	CreatedAt time.Time
	// RevokedAt is when the key was revoked, or nil while it still works.
	RevokedAt *time.Time
}

type principalKey struct{}

// WithPrincipal returns ctx carrying the authenticated Principal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the Principal a request was authenticated as.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
