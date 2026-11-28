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

// ErrConversationNotFound means no Conversation matched, or the Principal
// asking does not own the one that did.
var ErrConversationNotFound = errors.New("conversation not found")

type conversationRepository struct {
	store db.Store
}

// NewConversationRepository returns a ConversationRepository on store.
func NewConversationRepository(store db.Store) interfaces.ConversationRepository {
	return &conversationRepository{store: store}
}

// CreateConversation creates a Conversation owned by principalID.
func (r *conversationRepository) CreateConversation(ctx context.Context, principalID uuid.UUID) (*types.Conversation, error) {
	row, err := r.store.Querier(ctx).CreateConversation(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("create conversation for principal %s: %w", principalID, err)
	}
	return fromConversationRow(row), nil
}

// GetConversation returns the Conversation when principalID owns it.
func (r *conversationRepository) GetConversation(ctx context.Context, principalID, id uuid.UUID) (*types.Conversation, error) {
	row, err := r.store.Querier(ctx).GetConversation(ctx, sqlc.GetConversationParams{ID: id, PrincipalID: principalID})
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("conversation %s: %w", id, err)
	}
	return fromConversationRow(row), nil
}

// InsertConversationTurn appends turn and returns it with the Conversation's
// next Seq and Postgres' CreatedAt. The insert advances the Conversation's
// turn_seq under its row lock and matches only its owner; zero rows is
// ErrConversationNotFound. principalID comes first, the same order as
// GetConversation.
func (r *conversationRepository) InsertConversationTurn(ctx context.Context, principalID uuid.UUID,
	turn *types.ConversationTurn,
) (*types.ConversationTurn, error) {
	var rewrittenQuery string
	if turn.RewrittenQuery != nil {
		rewrittenQuery = *turn.RewrittenQuery
	}
	sourceDocumentIDs := turn.SourceDocumentIDs
	if sourceDocumentIDs == nil {
		sourceDocumentIDs = []uuid.UUID{}
	}
	row, err := r.store.Querier(ctx).InsertConversationTurn(ctx, sqlc.InsertConversationTurnParams{
		ConversationID:    turn.ConversationID,
		Question:          turn.Question,
		RewrittenQuery:    rewrittenQuery,
		SourceDocumentIds: sourceDocumentIDs,
		Answer:            turn.Answer,
		PrincipalID:       principalID,
	})
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("insert turn of conversation %s: %w", turn.ConversationID, err)
	}
	inserted := fromConversationTurnRow(row)
	return &inserted, nil
}

// ListConversationTurns returns every turn of the Conversation in Seq order.
// It trusts the caller's prior GetConversation ownership check.
func (r *conversationRepository) ListConversationTurns(ctx context.Context,
	conversationID uuid.UUID,
) ([]types.ConversationTurn, error) {
	rows, err := r.store.Querier(ctx).ListConversationTurns(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list turns of conversation %s: %w", conversationID, err)
	}
	return fromConversationTurnRows(rows), nil
}

// ListRecentConversationTurns returns the last limit turns in ascending Seq
// order, oldest first. It trusts the caller's prior GetConversation ownership
// check.
func (r *conversationRepository) ListRecentConversationTurns(ctx context.Context,
	conversationID uuid.UUID, limit int,
) ([]types.ConversationTurn, error) {
	rows, err := r.store.Querier(ctx).ListRecentConversationTurns(ctx, sqlc.ListRecentConversationTurnsParams{
		ConversationID: conversationID,
		TurnLimit:      int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list recent turns of conversation %s: %w", conversationID, err)
	}
	return fromConversationTurnRows(rows), nil
}

func fromConversationRow(row sqlc.Conversation) *types.Conversation {
	return &types.Conversation{
		ID:        row.ID,
		CreatedAt: row.CreatedAt,
	}
}

// fromConversationTurnRows renders rows as turns, always as a non-nil list.
func fromConversationTurnRows(rows []sqlc.ConversationTurn) []types.ConversationTurn {
	turns := make([]types.ConversationTurn, len(rows))
	for i, row := range rows {
		turns[i] = fromConversationTurnRow(row)
	}
	return turns
}

func fromConversationTurnRow(row sqlc.ConversationTurn) types.ConversationTurn {
	turn := types.ConversationTurn{
		ConversationID:    row.ConversationID,
		Seq:               row.Seq,
		Question:          row.Question,
		SourceDocumentIDs: row.SourceDocumentIds,
		Answer:            row.Answer,
		CreatedAt:         row.CreatedAt,
	}
	if turn.SourceDocumentIDs == nil {
		turn.SourceDocumentIDs = []uuid.UUID{}
	}
	if row.RewrittenQuery.Valid {
		query := row.RewrittenQuery.String
		turn.RewrittenQuery = &query
	}
	return turn
}
