package interfaces

import (
	"context"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types"
)

// AnswerService answers a question as a Pipeline Answer: retrieve the
// caller's readable Chunks, then generate one answer from them with
// citations. It answers with *errors.AppError for anything the client should
// see.
type AnswerService interface {
	// Answer validates req, retrieves through the configured
	// RetrievalService, calls the chat role once when anything was retrieved
	// and returns the answer with its citations; debug also returns the
	// retrieved Chunks, the trace and the token usage.
	Answer(ctx context.Context, req *types.AnswerRequest) (*types.AnswerResult, error)

	// StartAnswerStream validates req and retrieves exactly as Answer does,
	// and opens the model's stream before any event exists, so every failure
	// that can happen before generation, a provider rejection included, is
	// returned as an error. It then drains the stream in the background and
	// returns the answer ID whose events AnswerEvents streams.
	StartAnswerStream(ctx context.Context, req *types.AnswerRequest) (uuid.UUID, error)

	// AnswerEvents returns the Answer Events with Seq greater than afterSeq,
	// a channel closed when more arrive, and whether the stream has ended.
	// Only the Principal who started the stream may read it; any other caller
	// gets the same not-found error as for an unknown or expired answer ID.
	AnswerEvents(ctx context.Context, answerID uuid.UUID, afterSeq int64) (types.AnswerSubscription, error)

	// Close waits until the answer generation goroutines finish or ctx ends,
	// so a shutting-down process does not outlive the work they still hold.
	Close(ctx context.Context) error
}

// Generator is the chat model role the answer service calls. The adapter in
// internal/models/chat satisfies it; declaring it here keeps the application
// layer from depending on a model client.
type Generator interface {
	// Generate sends one non-streaming chat call and returns the reply.
	Generate(ctx context.Context, turns []types.ChatTurn) (*types.ChatResponse, error)

	// GenerateStream sends one streaming chat call. The channel follows
	// ChatStream's drain and cancel contract: the consumer drains it until it
	// closes, and cancels ctx when abandoning it.
	GenerateStream(ctx context.Context, turns []types.ChatTurn) (<-chan types.StreamResponse, error)

	// GetModelName returns the model name sent on the wire.
	GetModelName() string
}
