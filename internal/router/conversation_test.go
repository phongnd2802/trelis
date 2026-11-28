package router

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/service"
	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/handler"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// stubConversations answers the Conversation routes with fixed values and
// records what the handler asked for.
type stubConversations struct {
	created      *types.Conversation
	conversation *types.Conversation
	createErr    error
	getErr       error
	createCalls  int
	getCalls     int
	gotID        uuid.UUID
	gotIDs       []uuid.UUID
}

func (s *stubConversations) CreateConversation(context.Context) (*types.Conversation, error) {
	s.createCalls++
	return s.created, s.createErr
}

func (s *stubConversations) GetConversation(_ context.Context, id uuid.UUID) (*types.Conversation, error) {
	s.getCalls++
	s.gotID = id
	s.gotIDs = append(s.gotIDs, id)
	return s.conversation, s.getErr
}

var _ interfaces.ConversationService = (*stubConversations)(nil)

// newConversationRouter is the router on one ConversationService, with the
// other routes unmounted because these tests never call them.
func newConversationRouter(conversations interfaces.ConversationService) http.Handler {
	me := handler.NewPrincipalHandler(service.NewPrincipalService(nil, nil, nil))
	return New(Readiness{}, keyring{"trelis_alice": alice}, me, nil, nil, nil,
		handler.NewConversationHandler(conversations), nil, false)
}

func TestConversationRoutesNeedAKey(t *testing.T) {
	h := newConversationRouter(&stubConversations{})
	for _, authorization := range []string{"", "Bearer wrong"} {
		rec := postJSON(t, h, "/conversations", "", authorization)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST /conversations with %q = %d, want 401", authorization, rec.Code)
		}
		rec = get(t, h, "/conversations/"+uuid.NewString(), authorization)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET /conversations/{id} with %q = %d, want 401", authorization, rec.Code)
		}
	}
}

func TestCreateConversationAnswers201WithItsIDAndCreatedAt(t *testing.T) {
	conversationID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000c1")
	createdAt := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	svc := &stubConversations{created: &types.Conversation{ID: conversationID, CreatedAt: createdAt}}
	h := newConversationRouter(svc)

	for _, body := range []string{"", "{}"} {
		rec := postJSON(t, h, "/conversations", body, "Bearer trelis_alice")
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /conversations with body %q = %d %s, want 201", body, rec.Code, rec.Body)
		}
		var got struct {
			ID        uuid.UUID `json:"id"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(decode(t, rec).Data, &got); err != nil {
			t.Fatalf("data in %s is not a created Conversation: %v", rec.Body, err)
		}
		if got.ID != conversationID || !got.CreatedAt.Equal(createdAt) {
			t.Errorf("created = %+v, want id %s and created_at %s", got, conversationID, createdAt)
		}
		if strings.Contains(rec.Body.String(), `"turns"`) {
			t.Errorf("body = %s, want only id and created_at", rec.Body)
		}
	}
	if svc.createCalls != 2 {
		t.Errorf("CreateConversation called %d times, want once per request", svc.createCalls)
	}
}

func TestCreateConversationRejectsANonEmptyBody(t *testing.T) {
	h := newConversationRouter(&stubConversations{})
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"unknown field": {body: `{"turns":[]}`, code: int(apperrors.ErrValidation)},
		"not an object": {body: `[]`, code: int(apperrors.ErrBadRequest)},
		"two objects":   {body: `{}{}`, code: int(apperrors.ErrBadRequest)},
	} {
		t.Run(name, func(t *testing.T) {
			rec := postJSON(t, h, "/conversations", tc.body, "Bearer trelis_alice")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST /conversations %s = %d %s, want 400", tc.body, rec.Code, rec.Body)
			}
			errBody := decode(t, rec).Error
			if errBody == nil || errBody.Code != tc.code {
				t.Errorf("error = %+v, want code %d", errBody, tc.code)
			}
		})
	}
}

func TestGetConversationAnswersItsTurns(t *testing.T) {
	conversationID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000c1")
	createdAt := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	rewritten := "Thuế TNCN năm 2024"
	svc := &stubConversations{conversation: &types.Conversation{
		ID:        conversationID,
		CreatedAt: createdAt,
		Turns: []types.ConversationTurn{
			{
				Seq: 1, ConversationID: conversationID, Question: "Thuế TNCN là gì?",
				Answer: "Thuế thu nhập cá nhân.", CreatedAt: createdAt,
			},
			{
				Seq: 2, ConversationID: conversationID, Question: "và năm 2024 thì sao?",
				RewrittenQuery: &rewritten, Answer: "Năm 2024 thuế giảm.", CreatedAt: createdAt,
			},
		},
	}}
	h := newConversationRouter(svc)

	rec := get(t, h, "/conversations/"+conversationID.String(), "Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /conversations/%s = %d %s, want 200", conversationID, rec.Code, rec.Body)
	}
	if svc.gotID != conversationID {
		t.Errorf("service asked for %s, want %s", svc.gotID, conversationID)
	}
	var got types.Conversation
	if err := json.Unmarshal(decode(t, rec).Data, &got); err != nil {
		t.Fatalf("data in %s is not a Conversation: %v", rec.Body, err)
	}
	if got.ID != conversationID || !got.CreatedAt.Equal(createdAt) || len(got.Turns) != 2 {
		t.Fatalf("conversation = %+v, want its id, created_at and two turns", got)
	}
	if got.Turns[0].Question != "Thuế TNCN là gì?" || got.Turns[0].RewrittenQuery != nil {
		t.Errorf("turn 0 = %+v, want the raw question and no rewritten_query key", got.Turns[0])
	}
	if got.Turns[1].RewrittenQuery == nil || *got.Turns[1].RewrittenQuery != rewritten {
		t.Errorf("turn 1 rewritten_query = %v, want %q", got.Turns[1].RewrittenQuery, rewritten)
	}
	if strings.Contains(rec.Body.String(), `"conversation_id"`) {
		t.Errorf("body = %s, want a turn's conversation_id left out", rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"withheld"`) {
		t.Errorf("body = %s, want withheld omitted for readable turns", rec.Body)
	}
}

func TestGetConversationWithoutTurnsAnswersAnEmptyList(t *testing.T) {
	svc := &stubConversations{conversation: &types.Conversation{
		ID:        uuid.New(),
		CreatedAt: time.Now(),
		Turns:     []types.ConversationTurn{},
	}}
	h := newConversationRouter(svc)

	rec := get(t, h, "/conversations/"+svc.conversation.ID.String(), "Bearer trelis_alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /conversations = %d %s, want 200", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"turns":[]`) {
		t.Errorf("body = %s, want turns as [] rather than null", rec.Body)
	}
}

func TestGetConversationNotFoundIsTheSameBodyEverywhere(t *testing.T) {
	unknownID := uuid.New()
	svc := &stubConversations{getErr: apperrors.NewNotFoundError("Conversation not found")}
	h := newConversationRouter(svc)

	unknown := get(t, h, "/conversations/"+unknownID.String(), "Bearer trelis_alice")
	malformed := get(t, h, "/conversations/not-a-uuid", "Bearer trelis_alice")
	if unknown.Code != http.StatusNotFound || malformed.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d %s, malformed = %d %s; want 404 and 404",
			unknown.Code, unknown.Body, malformed.Code, malformed.Body)
	}
	if unknown.Body.String() != malformed.Body.String() {
		t.Errorf("unknown body = %s, malformed body = %s; want the same body",
			unknown.Body, malformed.Body)
	}
	if svc.getCalls != 2 {
		t.Errorf("GetConversation called %d times, want the unknown and the malformed id", svc.getCalls)
	}
	if len(svc.gotIDs) != 2 || svc.gotIDs[0] != unknownID || svc.gotIDs[1] != uuid.Nil {
		t.Errorf("service asked for %v, want %s then the zero UUID", svc.gotIDs, unknownID)
	}
}
