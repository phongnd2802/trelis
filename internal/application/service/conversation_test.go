package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
)

func TestCreateConversationBelongsToTheCaller(t *testing.T) {
	principalID := uuid.New()
	created := &types.Conversation{ID: uuid.New()}
	repo := &fakeConversations{created: created}
	svc := NewConversationService(repo, &fakeReadableDocuments{})

	got, err := svc.CreateConversation(callerContext(principalID))
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if got != created {
		t.Errorf("conversation = %+v, want the repository's %+v", got, created)
	}
	if repo.createdAs != principalID {
		t.Errorf("created for %s, want the caller %s", repo.createdAs, principalID)
	}
}

func TestGetConversationReturnsItsTurnsOldestFirst(t *testing.T) {
	principalID, conversationID := uuid.New(), uuid.New()
	repo := &fakeConversations{
		owned: true,
		turns: []types.ConversationTurn{
			{Seq: 1, ConversationID: conversationID, Question: "one", Answer: "first"},
			{Seq: 2, ConversationID: conversationID, Question: "two", Answer: "second"},
		},
	}
	svc := NewConversationService(repo, &fakeReadableDocuments{})

	got, err := svc.GetConversation(callerContext(principalID), conversationID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if repo.gotOwner != principalID || repo.gotID != conversationID {
		t.Errorf("GetConversation asked for owner/id %s/%s, want %s/%s",
			repo.gotOwner, repo.gotID, principalID, conversationID)
	}
	if len(got.Turns) != 2 || got.Turns[0].Seq != 1 || got.Turns[1].Seq != 2 {
		t.Errorf("turns = %+v, want the two turns in seq order", got.Turns)
	}
}

func TestAConversationWithoutTurnsAnswersAnEmptyList(t *testing.T) {
	repo := &fakeConversations{owned: true, turns: nil}
	svc := NewConversationService(repo, &fakeReadableDocuments{})

	got, err := svc.GetConversation(callerContext(uuid.New()), uuid.New())
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.Turns == nil || len(got.Turns) != 0 {
		t.Errorf("turns = %#v, want an empty non-nil list", got.Turns)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"turns":[]`) {
		t.Errorf("JSON = %s, want turns as [] rather than null", body)
	}
}

func TestAForeignOrUnknownConversationIsTheSameNotFound(t *testing.T) {
	repo := &fakeConversations{getErr: repository.ErrConversationNotFound}
	svc := NewConversationService(repo, &fakeReadableDocuments{})

	_, err := svc.GetConversation(callerContext(uuid.New()), uuid.New())
	appErr, ok := apperrors.IsAppError(err)
	if !ok || appErr.Code != apperrors.ErrNotFound || appErr.HTTPCode != 404 ||
		appErr.Message != "Conversation not found" {
		t.Errorf("GetConversation = %v, want the shared Conversation 404", err)
	}
}

func TestAConversationReadFailureIsNotANotFound(t *testing.T) {
	cause := errors.New("postgres down")
	svc := NewConversationService(&fakeConversations{getErr: cause}, &fakeReadableDocuments{})

	_, err := svc.GetConversation(callerContext(uuid.New()), uuid.New())
	if !errors.Is(err, cause) {
		t.Errorf("GetConversation = %v, want the database failure wrapped", err)
	}
	if _, ok := apperrors.IsAppError(err); ok {
		t.Errorf("GetConversation = %v, want a plain error so the handler answers 500", err)
	}
}

func TestTheConversationServiceNeedsACaller(t *testing.T) {
	svc := NewConversationService(&fakeConversations{owned: true}, &fakeReadableDocuments{})

	if _, err := svc.CreateConversation(context.Background()); err == nil {
		t.Error("CreateConversation without a Principal = nil, want an error")
	}
	if _, err := svc.GetConversation(context.Background(), uuid.New()); err == nil {
		t.Error("GetConversation without a Principal = nil, want an error")
	}
}

func TestGetConversationWithholdsATurnWhoseSourceIsUnreadable(t *testing.T) {
	principalID, conversationID := uuid.New(), uuid.New()
	readableDocument, restrictedDocument := uuid.New(), uuid.New()
	rewritten := "restricted query"
	repo := &fakeConversations{owned: true, turns: []types.ConversationTurn{
		{
			Seq: 1, ConversationID: conversationID, Question: "readable question",
			Answer: "readable answer", SourceDocumentIDs: []uuid.UUID{readableDocument},
		},
		{
			Seq: 2, ConversationID: conversationID, Question: "restricted question",
			RewrittenQuery: &rewritten, Answer: "restricted answer",
			SourceDocumentIDs: []uuid.UUID{restrictedDocument}, CreatedAt: time.Now(),
		},
		{
			Seq: 3, ConversationID: conversationID, Question: "no-sources question",
			Answer: "fixed answer", SourceDocumentIDs: []uuid.UUID{},
		},
	}}
	documents := &fakeReadableDocuments{readable: []uuid.UUID{readableDocument}}
	svc := NewConversationService(repo, documents)

	got, err := svc.GetConversation(callerContext(principalID), conversationID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if documents.calls != 1 || documents.gotPrincipal != principalID {
		t.Errorf("readable Documents resolved %d times for %s, want once for the caller %s",
			documents.calls, documents.gotPrincipal, principalID)
	}
	if len(got.Turns) != 3 {
		t.Fatalf("turns = %+v, want all three turns, withheld or not", got.Turns)
	}
	if turn := got.Turns[0]; turn.Withheld || turn.Answer != "readable answer" {
		t.Errorf("readable turn = %+v, want its answer shown", turn)
	}
	withheld := got.Turns[1]
	if !withheld.Withheld || withheld.Answer != "" || withheld.RewrittenQuery != nil {
		t.Errorf("withheld turn = %+v, want withheld with no answer or rewritten query", withheld)
	}
	if withheld.Seq != 2 || withheld.Question != "restricted question" || withheld.CreatedAt.IsZero() {
		t.Errorf("withheld turn metadata = %+v, want seq, question and created_at kept", withheld)
	}
	if turn := got.Turns[2]; turn.Withheld || turn.Answer != "fixed answer" {
		t.Errorf("no-sources turn = %+v, want it shown", turn)
	}
}

func TestAConversationReadFailureWhileResolvingAccessIs500(t *testing.T) {
	cause := errors.New("postgres down")
	svc := NewConversationService(&fakeConversations{owned: true},
		&fakeReadableDocuments{err: cause})

	_, err := svc.GetConversation(callerContext(uuid.New()), uuid.New())
	if !errors.Is(err, cause) {
		t.Errorf("GetConversation = %v, want the readable-Documents failure wrapped", err)
	}
	if _, ok := apperrors.IsAppError(err); ok {
		t.Errorf("GetConversation = %v, want a plain error so the handler answers 500", err)
	}
}
