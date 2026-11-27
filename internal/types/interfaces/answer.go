package interfaces

import (
	"context"

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
}

// Generator is the chat model role the answer service calls. The adapter in
// internal/models/chat satisfies it; declaring it here keeps the application
// layer from depending on a model client.
type Generator interface {
	// Generate sends one non-streaming chat call and returns the reply.
	Generate(ctx context.Context, turns []types.ChatTurn) (*types.ChatResponse, error)

	// GetModelName returns the model name sent on the wire.
	GetModelName() string
}
