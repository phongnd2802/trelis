package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// conversationTurn inserts one turn and fails the test when the insert does.
func conversationTurn(t *testing.T, repo interfaces.ConversationRepository, conversationID, principalID uuid.UUID,
	question, answer string, rewrittenQuery *string, sourceDocumentIDs ...uuid.UUID,
) *types.ConversationTurn {
	t.Helper()
	turn, err := repo.InsertConversationTurn(context.Background(), principalID, &types.ConversationTurn{
		ConversationID:    conversationID,
		Question:          question,
		RewrittenQuery:    rewrittenQuery,
		SourceDocumentIDs: sourceDocumentIDs,
		Answer:            answer,
	})
	if err != nil {
		t.Fatalf("insert turn %q: %v", question, err)
	}
	return turn
}

func TestCreateConversationThenGetItByOwner(t *testing.T) {
	s, ids := seeded(t)
	repo := NewConversationRepository(s)

	conversation, err := repo.CreateConversation(context.Background(), ids["alice"])
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if conversation.ID == uuid.Nil || conversation.CreatedAt.IsZero() {
		t.Errorf("conversation = %+v, want its id and created_at filled in", conversation)
	}

	got, err := repo.GetConversation(context.Background(), ids["alice"], conversation.ID)
	if err != nil {
		t.Fatalf("GetConversation by owner: %v", err)
	}
	if got.ID != conversation.ID || !got.CreatedAt.Equal(conversation.CreatedAt) {
		t.Errorf("got %+v, want %+v", got, conversation)
	}

	if _, err := repo.GetConversation(context.Background(), ids["bob"], conversation.ID); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("GetConversation by another Principal = %v, want ErrConversationNotFound", err)
	}
	if _, err := repo.GetConversation(context.Background(), ids["alice"], uuid.New()); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("GetConversation of an unknown id = %v, want ErrConversationNotFound", err)
	}
}

func TestConversationTurnsTakeTheNextSeq(t *testing.T) {
	s, ids := seeded(t)
	repo := NewConversationRepository(s)
	conversation, err := repo.CreateConversation(context.Background(), ids["alice"])
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	rewritten := "Thuế TNCN năm 2024"
	sourceDocumentID := uuid.New()
	first := conversationTurn(t, repo, conversation.ID, ids["alice"],
		"Thuế TNCN là gì?", "Thuế TNCN là thuế thu nhập cá nhân.", nil, sourceDocumentID)
	if first.Seq != 1 || first.ConversationID != conversation.ID || first.CreatedAt.IsZero() ||
		first.Question != "Thuế TNCN là gì?" || first.Answer != "Thuế TNCN là thuế thu nhập cá nhân." ||
		first.RewrittenQuery != nil {
		t.Errorf("first turn = %+v, want seq 1 with the raw question, answer and no rewritten query", first)
	}
	if len(first.SourceDocumentIDs) != 1 || first.SourceDocumentIDs[0] != sourceDocumentID {
		t.Errorf("first turn source Documents = %v, want %s", first.SourceDocumentIDs, sourceDocumentID)
	}
	second := conversationTurn(t, repo, conversation.ID, ids["alice"],
		"và năm 2024 thì sao?", "Năm 2024 thuế TNCN giảm.", &rewritten)
	if second.Seq != 2 {
		t.Errorf("second turn seq = %d, want 2", second.Seq)
	}
	if second.RewrittenQuery == nil || *second.RewrittenQuery != rewritten {
		t.Errorf("second turn rewritten query = %v, want %q", second.RewrittenQuery, rewritten)
	}

	if _, err := repo.InsertConversationTurn(context.Background(), ids["bob"], &types.ConversationTurn{
		ConversationID: conversation.ID,
		Question:       "who won?",
		Answer:         "someone",
	}); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("insert by another Principal = %v, want ErrConversationNotFound", err)
	}
	if _, err := repo.InsertConversationTurn(context.Background(), ids["alice"], &types.ConversationTurn{
		ConversationID: uuid.New(),
		Question:       "who won?",
		Answer:         "someone",
	}); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("insert into an unknown Conversation = %v, want ErrConversationNotFound", err)
	}
}

func TestListRecentConversationTurnsReturnsTheLastOnesOldestFirst(t *testing.T) {
	s, ids := seeded(t)
	repo := NewConversationRepository(s)
	conversation, err := repo.CreateConversation(context.Background(), ids["alice"])
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	for i, question := range []string{"one", "two", "three", "four"} {
		conversationTurn(t, repo, conversation.ID, ids["alice"], question, "answer", nil)
		if i == 0 {
			// A second Conversation's turns must not leak into this one.
			other, err := repo.CreateConversation(context.Background(), ids["alice"])
			if err != nil {
				t.Fatalf("CreateConversation: %v", err)
			}
			conversationTurn(t, repo, other.ID, ids["alice"], "other", "answer", nil)
		}
	}

	all, err := repo.ListConversationTurns(context.Background(), conversation.ID)
	if err != nil {
		t.Fatalf("ListConversationTurns: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("listed %d turns, want 4", len(all))
	}
	for i, turn := range all {
		if turn.Seq != int64(i+1) {
			t.Errorf("turn %d seq = %d, want %d in ascending order", i, turn.Seq, i+1)
		}
	}

	recent, err := repo.ListRecentConversationTurns(context.Background(), conversation.ID, 2)
	if err != nil {
		t.Fatalf("ListRecentConversationTurns: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("listed %d recent turns, want 2", len(recent))
	}
	if recent[0].Seq != 3 || recent[1].Seq != 4 {
		t.Errorf("recent seqs = %d, %d; want 3, 4 (the last two, oldest first)", recent[0].Seq, recent[1].Seq)
	}
	if recent[0].Question != "three" || recent[1].Question != "four" {
		t.Errorf("recent questions = %q, %q; want three, four", recent[0].Question, recent[1].Question)
	}

	empty, err := repo.ListConversationTurns(context.Background(), uuid.New())
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("ListConversationTurns(unknown) = %#v, %v; want an empty, non-nil list", empty, err)
	}
}
