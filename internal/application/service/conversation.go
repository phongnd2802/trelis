package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

type conversationService struct {
	repo interfaces.ConversationRepository
	// documents resolves what the caller can read now, so a turn's answer is
	// shown only while every Document it was generated from is readable
	// (ADR-0004).
	documents interfaces.DocumentRepository
}

// NewConversationService returns the Conversation use cases.
func NewConversationService(repo interfaces.ConversationRepository,
	documents interfaces.DocumentRepository,
) interfaces.ConversationService {
	return &conversationService{repo: repo, documents: documents}
}

// CreateConversation creates a Conversation owned by the caller.
func (s *conversationService) CreateConversation(ctx context.Context) (*types.Conversation, error) {
	principal, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.CreateConversation(ctx, principal.ID)
}

// GetConversation returns the caller's Conversation with its turns in Seq
// order. An unknown id, another Principal's Conversation and a deleted one
// answer the same not-found error, so nothing about a foreign Conversation
// leaks. A turn whose source Documents are no longer all readable is withheld
// instead of answered.
func (s *conversationService) GetConversation(ctx context.Context, id uuid.UUID) (*types.Conversation, error) {
	principal, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	conversation, err := s.repo.GetConversation(ctx, principal.ID, id)
	if errors.Is(err, repository.ErrConversationNotFound) {
		return nil, errConversationNotFound()
	}
	if err != nil {
		return nil, err
	}
	turns, err := s.repo.ListConversationTurns(ctx, id)
	if err != nil {
		return nil, err
	}
	readable, err := readableDocumentSet(ctx, s.documents, principal.ID)
	if err != nil {
		return nil, err
	}
	withholdUnreadableTurns(turns, readable)
	if turns == nil {
		turns = []types.ConversationTurn{}
	}
	conversation.Turns = turns
	return conversation, nil
}

// readableDocumentSet returns the IDs of every Document principalID can read
// now. A turn's answer is derived data, so each read resolves this set afresh
// and never caches it (ADR-0004).
func readableDocumentSet(ctx context.Context, documents interfaces.DocumentRepository,
	principalID uuid.UUID,
) (map[uuid.UUID]struct{}, error) {
	ids, err := documents.ListReadableDocumentIDs(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("list readable documents: %w", err)
	}
	readable := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		readable[id] = struct{}{}
	}
	return readable, nil
}

// withholdUnreadableTurns clears the derived data of a turn when any source
// Document is missing from readable: the answer becomes empty and the
// rewritten query is dropped, while Seq, Question and CreatedAt stay. A turn
// with no source Document is always shown; a no-sources answer that did not
// rewrite has none, while one whose query was rewritten inherits the history
// sources and can be withheld.
func withholdUnreadableTurns(turns []types.ConversationTurn, readable map[uuid.UUID]struct{}) {
	for i := range turns {
		turn := &turns[i]
		for _, documentID := range turn.SourceDocumentIDs {
			if _, ok := readable[documentID]; ok {
				continue
			}
			turn.Withheld = true
			turn.Answer = ""
			turn.RewrittenQuery = nil
			break
		}
	}
}

// errConversationNotFound is what every route answers for a Conversation that
// is missing or another Principal's: one body, so the caller learns nothing
// about which it was.
func errConversationNotFound() error {
	return apperrors.NewNotFoundError("Conversation not found")
}
