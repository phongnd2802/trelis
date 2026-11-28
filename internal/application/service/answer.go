package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"unicode"

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
	// streams buffers the events of answers being generated or retained for
	// resume (ADR-0010).
	streams *answerStreamHub
	// generationTimeout bounds one streamed answer's generation.
	generationTimeout time.Duration
	// generations tracks the streamed answers still draining, so Close can
	// wait for them at shutdown.
	generations sync.WaitGroup
}

// NewAnswerService returns the Pipeline Answer use case. generator may be nil
// when no chat model is configured; an answer then fails with a
// service-unavailable error before any retrieval.
func NewAnswerService(retrieval interfaces.RetrievalService, generator interfaces.Generator) interfaces.AnswerService {
	return &answerService{
		retrieval:         retrieval,
		generator:         generator,
		streams:           newAnswerStreamHub(answerStreamRetention, answerStreamMaxEvents),
		generationTimeout: answerGenerationTimeout,
	}
}

// prepare runs the steps both answers share before generation: identify the
// caller, validate the request, require a configured chat role and retrieve
// the readable Chunks. Its errors are the ones the JSON answer returns
// unchanged, so the streamed path fails before any event for the same
// reasons.
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

// Answer validates req, retrieves through the retrieval service (whose errors
// pass through unchanged), and calls the chat role once with the numbered
// sources and the question. It keeps only the citation markers that name a
// retrieved source and answers each cited source once, in order of first
// appearance. With nothing retrieved it answers a fixed sentence in the
// question's language and makes no chat call. When req.Debug is set the
// result also carries the retrieved Chunks, the trace ending with the
// generate step, and the chat call's token usage.
func (s *answerService) Answer(ctx context.Context, req *types.AnswerRequest) (*types.AnswerResult, error) {
	_, retrieved, err := s.prepare(ctx, req)
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
		reply, err := s.generator.Generate(ctx, answerTurns(req.Query, retrieved.Results))
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

	result := &types.AnswerResult{
		Answer:       answer,
		Citations:    citations,
		FinishReason: finishReason,
	}
	if req.Debug {
		result.Debug = &types.AnswerDebug{
			Results: retrieved.Results,
			Trace:   trace,
			Usage:   usage,
		}
	}
	return result, nil
}
