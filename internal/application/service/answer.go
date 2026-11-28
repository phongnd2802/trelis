package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// noSourcesAnswer is the answer, in English, when retrieval found no Chunk the
// caller can read. No chat call is made for it, so the sentence is fixed
// rather than generated.
const noSourcesAnswer = "I could not find anything you can read that answers this question."

// noSourcesAnswerVietnamese is noSourcesAnswer for a Vietnamese question, the
// language the prompt answers in and the Demo Corpus speaks.
const noSourcesAnswerVietnamese = "Tôi không tìm thấy thông tin nào bạn được phép đọc để trả lời câu hỏi này."

// noSourcesReply returns the fixed answer for a question retrieval found
// nothing for, in the question's language: Vietnamese when the query uses
// letters specific to Vietnamese, English otherwise.
func noSourcesReply(query string) string {
	if hasVietnameseLetters(query) {
		return noSourcesAnswerVietnamese
	}
	return noSourcesAnswer
}

// hasVietnameseLetters reports whether s contains a letter specific to
// Vietnamese: đ, ă, â, ê, ô, ơ, ư or a vowel carrying a tone mark. A query
// typed without diacritics such as "thue tncn" has none, so it is treated as
// English.
func hasVietnameseLetters(s string) bool {
	for _, r := range s {
		switch r {
		case 'đ', 'Đ', 'ă', 'Ă', 'ơ', 'Ơ', 'ư', 'Ư':
			return true
		}
		// â, ê and ô are in the tone-marked ranges below as well.
		if unicode.Is(unicode.Latin, r) &&
			((r >= '\u00C0' && r <= '\u00C3') ||
				(r >= '\u00C8' && r <= '\u00CA') ||
				(r >= '\u00CC' && r <= '\u00CD') ||
				(r >= '\u00D2' && r <= '\u00D5') ||
				(r >= '\u00D9' && r <= '\u00DA') ||
				r == '\u00DD' ||
				(r >= '\u00E0' && r <= '\u00E3') ||
				(r >= '\u00E8' && r <= '\u00EA') ||
				(r >= '\u00EC' && r <= '\u00ED') ||
				(r >= '\u00F2' && r <= '\u00F5') ||
				(r >= '\u00F9' && r <= '\u00FA') ||
				r == '\u00FD' ||
				(r >= '\u1EA0' && r <= '\u1EF9')) {
			return true
		}
	}
	return false
}

// answerService produces a Pipeline Answer: retrieve the caller's readable
// Chunks, then generate one answer from them with citations.
type answerService struct {
	retrieval interfaces.RetrievalService
	// generator is nil when models.chat is not configured, which makes an
	// answer service unavailable rather than a fixed sentence pretending a
	// model answered.
	generator interfaces.Generator
	// conversations loads the Conversation History a follow-up carries and
	// records each turn of a conversation the answer belongs to.
	conversations interfaces.ConversationRepository
	// documents resolves what the caller can read now, so a withheld turn is
	// left out of the history (ADR-0004).
	documents interfaces.DocumentRepository
	// streams buffers the events of answers being generated or retained for
	// resume (ADR-0010).
	streams *answerStreamHub
	// generationTimeout bounds one streamed answer's generation.
	generationTimeout time.Duration
	// generations tracks the streamed answers still draining, so Close can
	// wait for them at shutdown.
	generations sync.WaitGroup
}

// conversationHistoryTurns is how many earlier turns of a Conversation a
// follow-up question carries into retrieval.
const conversationHistoryTurns = 5

// NewAnswerService returns the Pipeline Answer use case. generator may be nil
// when no chat model is configured; an answer then fails with a
// service-unavailable error before any retrieval. conversations and documents
// may be nil only when no request carries a conversation_id.
func NewAnswerService(retrieval interfaces.RetrievalService, generator interfaces.Generator,
	conversations interfaces.ConversationRepository, documents interfaces.DocumentRepository,
) interfaces.AnswerService {
	return &answerService{
		retrieval:         retrieval,
		generator:         generator,
		conversations:     conversations,
		documents:         documents,
		streams:           newAnswerStreamHub(answerStreamRetention, answerStreamMaxEvents),
		generationTimeout: answerGenerationTimeout,
	}
}

// prepare runs the steps both answers share before generation: identify the
// caller, validate the request, require a configured chat role, load the
// Conversation History when the question belongs to a Conversation and
// retrieve the readable Chunks. Its errors are the ones the JSON answer
// returns unchanged, so the streamed path fails before any event for the same
// reasons. A conversation_id the caller does not own is the same not-found
// error as an unknown one, returned before any retrieval or model call.
func (s *answerService) prepare(ctx context.Context, req *types.AnswerRequest) (
	types.Principal, *types.RetrievalResult, error,
) {
	principal, err := caller(ctx)
	if err != nil {
		return types.Principal{}, nil, err
	}
	if err := req.Validate(); err != nil {
		return types.Principal{}, nil, apperrors.NewValidationError(err.Error())
	}
	// Ownership comes before the chat-role check, so a foreign Conversation
	// is the same 404 whether or not models.chat is configured.
	if req.ConversationID != nil {
		if err := s.loadHistory(ctx, principal.ID, req); err != nil {
			return types.Principal{}, nil, err
		}
	}
	if s.generator == nil {
		return types.Principal{}, nil, apperrors.NewServiceUnavailableError(
			"pipeline answers need models.chat to be configured")
	}
	retrieved, err := s.retrieval.Retrieve(ctx, &req.RetrievalRequest)
	if err != nil {
		return types.Principal{}, nil, err
	}
	return principal, retrieved, nil
}

// loadHistory checks that principalID owns req's Conversation and fills
// req.History with its recent turns whose source Documents are all readable
// now, oldest first. The last conversationHistoryTurns are loaded first and
// withheld turns are then dropped, so a follow-up may carry fewer than five
// when an earlier answer is no longer readable. An unknown or foreign
// Conversation is the one not-found error the routes answer, so its existence
// never leaks.
func (s *answerService) loadHistory(ctx context.Context, principalID uuid.UUID, req *types.AnswerRequest) error {
	if _, err := s.conversations.GetConversation(ctx, principalID, *req.ConversationID); err != nil {
		if errors.Is(err, repository.ErrConversationNotFound) {
			return errConversationNotFound()
		}
		return err
	}
	turns, err := s.conversations.ListRecentConversationTurns(ctx, *req.ConversationID, conversationHistoryTurns)
	if err != nil {
		return err
	}
	readable, err := readableDocumentSet(ctx, s.documents, principalID)
	if err != nil {
		return err
	}
	withholdUnreadableTurns(turns, readable)
	req.History = make([]types.ConversationTurn, 0, len(turns))
	for _, turn := range turns {
		if turn.Withheld {
			continue
		}
		req.History = append(req.History, turn)
	}
	return nil
}

// recordTurn appends req's question and the cleaned answer to its
// Conversation, with the rewritten query when a query rewrite ran and the
// distinct Documents the turn depends on: every retrieved result, plus, when
// a rewrite ran, the source Documents of the history answers it was rewritten
// from. A request without a conversation_id records nothing. The insert
// matches only the Conversation's owner; any failure is the caller's 500,
// since ownership was already checked before retrieval.
func (s *answerService) recordTurn(ctx context.Context, principalID uuid.UUID, req *types.AnswerRequest,
	retrieved *types.RetrievalResult, answer string,
) error {
	if req.ConversationID == nil {
		return nil
	}
	turn := &types.ConversationTurn{
		ConversationID:    *req.ConversationID,
		Question:          req.Query,
		SourceDocumentIDs: turnSourceDocumentIDs(retrieved, req.History),
		Answer:            answer,
	}
	if retrieved.RewrittenQuery != nil {
		rewritten := *retrieved.RewrittenQuery
		turn.RewrittenQuery = &rewritten
	}
	if _, err := s.conversations.InsertConversationTurn(ctx, principalID, turn); err != nil {
		return fmt.Errorf("record turn of conversation %s: %w", *req.ConversationID, err)
	}
	return nil
}

// sourceDocumentIDs returns the distinct Document IDs of every retrieved
// result, in first-appearance order. It covers every result, not only the
// cited ones, because the answer was generated from them all.
func sourceDocumentIDs(results []types.RetrievedChunk) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(results))
	seen := make(map[uuid.UUID]struct{}, len(results))
	for _, result := range results {
		if _, ok := seen[result.DocumentID]; ok {
			continue
		}
		seen[result.DocumentID] = struct{}{}
		ids = append(ids, result.DocumentID)
	}
	return ids
}

// turnSourceDocumentIDs returns the distinct Documents a recorded turn
// depends on. It starts with this turn's retrieved Documents. When a rewrite
// ran, the rewritten query was built from the history answers, so their
// source Documents are inherited too; without a rewrite the history never
// reached the turn. Withheld history turns are already absent from history and
// contribute nothing.
func turnSourceDocumentIDs(retrieved *types.RetrievalResult, history []types.ConversationTurn) []uuid.UUID {
	ids := sourceDocumentIDs(retrieved.Results)
	if retrieved.RewrittenQuery == nil {
		return ids
	}
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	for _, turn := range history {
		for _, id := range turn.SourceDocumentIDs {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

// addUsage returns a plus b, so a debug object can sum the token usage of
// every chat call an answer made, retrieval's and its own.
func addUsage(a, b types.TokenUsage) types.TokenUsage {
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.TotalTokens += b.TotalTokens
	return a
}

// Answer validates req, retrieves through the retrieval service (whose errors
// pass through unchanged), and calls the chat role once with the numbered
// sources and the question. It keeps only the citation markers that name a
// retrieved source and answers each cited source once, in order of first
// appearance. With nothing retrieved it answers a fixed sentence in the
// question's language and makes no chat call. When req.Debug is set the
// result also carries the retrieved Chunks, the trace ending with the
// generate step, and the token usage of every model call the answer made,
// including a query rewrite or decomposition.
func (s *answerService) Answer(ctx context.Context, req *types.AnswerRequest) (*types.AnswerResult, error) {
	principal, retrieved, err := s.prepare(ctx, req)
	if err != nil {
		return nil, err
	}

	var (
		answer       = noSourcesReply(req.Query)
		citations    = []types.Citation{}
		finishReason string
		usage        types.TokenUsage
		trace        = retrieved.Trace
	)
	if len(retrieved.Results) > 0 {
		start := time.Now()
		reply, err := s.generator.Generate(ctx, answerTurns(retrieved.Query, retrieved.Results))
		duration := time.Since(start)
		if err != nil {
			return nil, fmt.Errorf("generate pipeline answer: %w", err)
		}
		if reply == nil {
			return nil, errors.New("generate pipeline answer: the chat model returned no reply")
		}
		parsed, markers := parseCitations(reply.Content, len(retrieved.Results))
		answer = parsed
		citations = citationList(markers, retrieved.Results)
		finishReason = reply.FinishReason
		usage = reply.Usage
		trace = append(slices.Clone(trace), types.TraceStep{
			Step:       types.TraceStepGenerate,
			DurationMS: duration.Milliseconds(),
			Model:      s.generator.GetModelName(),
		})
	}
	if err := s.recordTurn(ctx, principal.ID, req, retrieved, answer); err != nil {
		return nil, err
	}

	result := &types.AnswerResult{
		Answer:       answer,
		Citations:    citations,
		FinishReason: finishReason,
	}
	if req.Debug {
		result.Debug = &types.AnswerDebug{
			Results: retrieved.Results,
			Trace:   trace,
			Usage:   addUsage(usage, retrieved.ChatUsage),
		}
	}
	return result, nil
}
