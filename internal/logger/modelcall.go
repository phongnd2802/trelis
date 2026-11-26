package logger

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// ModelCall is the metadata of one logical model call: one HTTP request, not
// one retry attempt. It never carries prompts, messages, inputs, documents or
// completions, and deliberately no error value: error strings can carry
// vendor bodies that echo the request, so the boundary logs the error once.
type ModelCall struct {
	// Role is the config role ("chat", "extraction", "judge", "embedding",
	// "rerank") when the caller knows it.
	Role     string
	Model    string
	Protocol string
	Duration time.Duration
	// Attempts is how many HTTP sends the call took, retries included.
	Attempts int
	// Failed marks a call that returned no usable reply.
	Failed bool
	// HTTPStatus is the vendor status when the failure was an *api.HTTPError.
	HTTPStatus int
	// HasUsage is true when the provider reported input token counts.
	HasUsage bool
	// HasOutputTokens is true when output_tokens is meaningful (chat replies).
	HasOutputTokens bool
	InputTokens     int
	OutputTokens    int
}

// LogModelCall writes the one INFO line for a model call, successful or not.
func LogModelCall(ctx context.Context, c ModelCall) {
	fields := make([]zap.Field, 0, 11)
	if c.Role != "" {
		fields = append(fields, zap.String("role", c.Role))
	}
	status := "ok"
	if c.Failed {
		status = "error"
	}
	fields = append(fields,
		zap.String("model", c.Model),
		zap.String("protocol", c.Protocol),
		zap.String("status", status),
		zap.Int64("duration_ms", c.Duration.Milliseconds()),
		zap.Int("attempts", c.Attempts),
	)
	if c.HTTPStatus != 0 {
		fields = append(fields, zap.Int("http_status", c.HTTPStatus))
	}
	if c.HasUsage {
		fields = append(fields, zap.Int("input_tokens", c.InputTokens))
		if c.HasOutputTokens {
			fields = append(fields, zap.Int("output_tokens", c.OutputTokens))
		}
	}
	Ctx(ctx).Info("model call", fields...)
}
