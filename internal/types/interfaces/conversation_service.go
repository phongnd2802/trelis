package interfaces

import (
	"context"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// ConversationService is the Conversation use cases the HTTP handler calls.
// It answers with *errors.AppError for anything the client should see, and a
// Conversation another Principal owns is the same not-found error as an
// unknown id.
type ConversationService interface {
	// CreateConversation creates a Conversation owned by the caller.
	CreateConversation(ctx context.Context) (*types.Conversation, error)
	// GetConversation returns the caller's Conversation with its turns in Seq
	// order. An unknown id, another Principal's Conversation and a malformed
	// id all answer the same not-found error.
	GetConversation(ctx context.Context, id uuid.UUID) (*types.Conversation, error)
}
