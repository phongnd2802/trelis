package interfaces

import (
	"context"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// ConversationRepository persists Conversations and their turns. Create, Get
// and InsertConversationTurn are scoped to one Principal, so a Conversation
// another Principal owns is the repository's not-found error, never a
// forbidden one. The two list methods take no Principal and trust the caller's
// prior GetConversation ownership check.
type ConversationRepository interface {
	// CreateConversation creates a Conversation owned by principalID.
	CreateConversation(ctx context.Context, principalID uuid.UUID) (*types.Conversation, error)
	// GetConversation returns the Conversation when principalID owns it;
	// ErrConversationNotFound covers both an unknown id and another
	// Principal's Conversation.
	GetConversation(ctx context.Context, principalID, id uuid.UUID) (*types.Conversation, error)
	// InsertConversationTurn appends turn to its Conversation and returns it
	// with the Conversation's next Seq and the CreatedAt Postgres assigned.
	// The insert advances the Conversation's counter under its row lock and
	// only matches its owner, so ErrConversationNotFound covers an unknown
	// Conversation and another Principal's alike. principalID comes first,
	// the same order as GetConversation.
	InsertConversationTurn(ctx context.Context, principalID uuid.UUID, turn *types.ConversationTurn) (*types.ConversationTurn, error)
	// ListConversationTurns returns every turn of the Conversation in Seq
	// order. A Conversation with no turns gives an empty list, not an error.
	// It trusts the caller's prior GetConversation ownership check.
	ListConversationTurns(ctx context.Context, conversationID uuid.UUID) ([]types.ConversationTurn, error)
	// ListRecentConversationTurns returns the last limit turns of the
	// Conversation in ascending Seq order, oldest first. A Conversation with
	// no turns gives an empty list, not an error. It trusts the caller's prior
	// GetConversation ownership check.
	ListRecentConversationTurns(ctx context.Context, conversationID uuid.UUID, limit int) ([]types.ConversationTurn, error)
}
