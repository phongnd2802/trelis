package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/shutdown"
	"github.com/phongnd2802/trelis/internal/types"
)

const (
	// answerGenerationTimeout bounds one answer's generation, so a provider
	// that never finishes cannot keep the goroutine for the process's
	// lifetime after its client has gone.
	answerGenerationTimeout = 5 * time.Minute
	// answerStreamErrorMessage is the one client-facing message for a failed
	// streamed answer. The cause goes to the log, never into the event.
	answerStreamErrorMessage = "The answer could not be completed. Please try again."
)

// StartAnswerStream validates req and retrieves exactly as Answer does, and
// opens the chat stream before any event exists, so every failure that can
// happen before generation, a provider rejection included, is returned before
// the SSE headers. It then drains the stream in the background and returns the
// answer ID whose events AnswerEvents streams.
//
// Generation does not inherit the request's cancellation: a client that drops
// after the first token must not stop it, or a resume would find a truncated
// answer. It keeps the request's values, including the router's shutdown
// notice, so the process shutdown still cancels it, and it is bounded by
// answerGenerationTimeout.
func (s *answerService) StartAnswerStream(ctx context.Context, req *types.AnswerRequest) (uuid.UUID, error) {
	principal, retrieved, err := s.prepare(ctx, req)
	if err != nil {
		return uuid.Nil, err
	}
	if len(retrieved.Results) == 0 {
		return s.startNoSourcesStream(principal.ID, req, retrieved), nil
	}

	genCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.generationTimeout)
	stopWatching := watchShutdown(genCtx, cancel)
	start := time.Now()
	stream, err := s.generator.GenerateStream(genCtx, answerTurns(req.Query, retrieved.Results))
	if err != nil {
		stopWatching()
		cancel()
		return uuid.Nil, err
	}

	id := s.streams.start(principal.ID)
	s.generations.Add(1)
	go func() {
		defer s.generations.Done()
		defer cancel()
		defer stopWatching()
		s.generate(genCtx, id, retrieved, stream, start, req.Debug)
	}()
	return id, nil
}

// startNoSourcesStream buffers the fixed no-source answer without calling the
// chat role, so the handler finds started, one token and done already there.
func (s *answerService) startNoSourcesStream(owner uuid.UUID, req *types.AnswerRequest,
	retrieved *types.RetrievalResult,
) uuid.UUID {
	id := s.streams.start(owner)
	answer := noSourcesReply(req.Query)
	s.streams.append(id, types.AnswerEventToken, types.AnswerTokenEvent{Text: answer})
	s.streams.append(id, types.AnswerEventDone, types.AnswerDoneEvent{
		Answer: answer,
		Debug:  answerDebug(retrieved, types.TraceStep{}, types.TokenUsage{}, req.Debug),
	})
	return id
}

// AnswerEvents returns the Answer Events of answerID after afterSeq. Only the
// Principal who started the answer may read them; any other caller gets the
// same not-found error as for an unknown or expired ID.
func (s *answerService) AnswerEvents(ctx context.Context, answerID uuid.UUID, afterSeq int64) (types.AnswerSubscription, error) {
	principal, err := caller(ctx)
	if err != nil {
		return types.AnswerSubscription{}, err
	}
	return s.streams.subscribe(answerID, principal.ID, afterSeq)
}

// Close waits until the answer generation goroutines finish or ctx ends, so a
// shutting-down process does not return while they still run.
func (s *answerService) Close(ctx context.Context) error {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.generations.Wait()
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// answerReply is what one drained chat stream produced, for finishAnswer.
type answerReply struct {
	text         string
	finishReason string
	usage        types.TokenUsage
	duration     time.Duration
	debug        bool
}

// generate drains one open chat stream into the hub. It emits token events as
// the chat role produces text, one citation event per cited source after the
// text, and one terminal done or error event. Failure is logged here, once,
// where it becomes the error event; content never reaches the log.
func (s *answerService) generate(ctx context.Context, id uuid.UUID, retrieved *types.RetrievalResult,
	stream <-chan types.StreamResponse, start time.Time, debug bool,
) {
	defer s.streams.finish(id)

	var (
		text         strings.Builder
		finishReason string
		usage        types.TokenUsage
	)
	for {
		select {
		case <-ctx.Done():
			s.failOrStop(ctx, id, fmt.Errorf("generate streamed pipeline answer: %w", ctx.Err()))
			return
		case chunk, ok := <-stream:
			if !ok {
				s.failOrStop(ctx, id, errors.New("generate streamed pipeline answer: the chat model closed the stream without a closing chunk"))
				return
			}
			switch chunk.ResponseType {
			case types.ResponseTypeThinking, types.ResponseTypeToolCall:
				// Thinking chunks are not answer text, and the answer prompt
				// offers no tools, so neither becomes a token event.
			case types.ResponseTypeError:
				s.failOrStop(ctx, id, errors.New("generate streamed pipeline answer: the chat stream ended with an error"))
				return
			case types.ResponseTypeAnswer:
				if chunk.Content != "" {
					text.WriteString(chunk.Content)
					if !s.streams.append(id, types.AnswerEventToken, types.AnswerTokenEvent{Text: chunk.Content}) {
						return
					}
				}
				if !chunk.Done {
					continue
				}
				if chunk.Usage != nil {
					usage = *chunk.Usage
				}
				finishReason = chunk.FinishReason
				s.finishAnswer(id, retrieved, answerReply{
					text:         text.String(),
					finishReason: finishReason,
					usage:        usage,
					duration:     time.Since(start),
					debug:        debug,
				})
				return
			}
		}
	}
}

// finishAnswer parses the full reply, emits one citation event per cited
// source and the terminal done event.
func (s *answerService) finishAnswer(id uuid.UUID, retrieved *types.RetrievalResult, reply answerReply) {
	answer, markers := parseCitations(reply.text, len(retrieved.Results))
	for _, citation := range citationList(markers, retrieved.Results) {
		if !s.streams.append(id, types.AnswerEventCitation, types.AnswerCitationEvent(citation)) {
			return
		}
	}
	s.streams.append(id, types.AnswerEventDone, types.AnswerDoneEvent{
		Answer:       answer,
		FinishReason: reply.finishReason,
		Debug: answerDebug(retrieved, types.TraceStep{
			Step:       types.TraceStepGenerate,
			DurationMS: reply.duration.Milliseconds(),
			Model:      s.generator.GetModelName(),
		}, reply.usage, reply.debug),
	})
}

// failOrStop logs and ends a failed generation, at info when the server is
// shutting down instead of at error.
func (s *answerService) failOrStop(ctx context.Context, id uuid.UUID, cause error) {
	if shutdownStarted(ctx) {
		s.stopAtShutdown(ctx, id)
		return
	}
	s.fail(ctx, id, cause)
}

// fail logs cause once and ends the stream with the generic client message.
// The cause is a fixed summary, never a model response body.
func (s *answerService) fail(ctx context.Context, id uuid.UUID, cause error) {
	logger.Ctx(ctx).Error("answer stream generation failed",
		zap.String("role", "chat"), zap.Stringer("answer", id), zap.Error(cause))
	s.streams.append(id, types.AnswerEventError, types.AnswerErrorEvent{Message: answerStreamErrorMessage})
}

// stopAtShutdown ends the stream when the server is going away. That is not a
// generation failure, so it is logged at info.
func (s *answerService) stopAtShutdown(ctx context.Context, id uuid.UUID) {
	logger.Ctx(ctx).Info("answer stream generation stopped at shutdown", zap.Stringer("answer", id))
	s.streams.append(id, types.AnswerEventError, types.AnswerErrorEvent{Message: answerStreamErrorMessage})
}

// answerDebug is the debug object of a done event, mirroring AnswerDebug: the
// retrieved Chunks, the retrieval trace plus generate when the chat role ran,
// and the call's usage. It is nil when the request did not ask for debug.
func answerDebug(retrieved *types.RetrievalResult, generate types.TraceStep,
	usage types.TokenUsage, debug bool,
) *types.AnswerDebug {
	if !debug {
		return nil
	}
	trace := retrieved.Trace
	if generate.Step != "" {
		trace = append(trace[:len(trace):len(trace)], generate)
	}
	return &types.AnswerDebug{Results: retrieved.Results, Trace: trace, Usage: usage}
}

// watchShutdown cancels a generation when the server begins shutting down,
// even while it is blocked in GenerateStream waiting for the model's headers.
// The returned function stops watching.
func watchShutdown(ctx context.Context, cancel context.CancelFunc) (stop func()) {
	signal := shutdown.Signal(ctx)
	if signal == nil {
		return func() {}
	}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-signal:
			cancel()
		case <-stopped:
		}
	}()
	return func() { close(stopped) }
}

// shutdownStarted reports whether ctx's server has begun shutting down.
func shutdownStarted(ctx context.Context) bool {
	signal := shutdown.Signal(ctx)
	if signal == nil {
		return false
	}
	select {
	case <-signal:
		return true
	default:
		return false
	}
}
