package ingest

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.uber.org/zap"

	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/models/api"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// Temporal application error types. The workflow checks these to decide what
// to do with a failure and what may be shown to a Document's readers; only
// an unparseable Source File and an unconfigured embedding model have a
// message chosen for that.
const (
	// unparseableDocumentType marks a Source File the parser cannot read.
	unparseableDocumentType = "unparseable_document"
	// embeddingUnconfiguredType marks a Document that cannot be embedded
	// because no embedding model is configured.
	embeddingUnconfiguredType = "embedding_unconfigured"
	// documentNotFoundType marks a Document that no longer exists; there is
	// no row left to mark failed.
	documentNotFoundType = "document_not_found"
	// invalidDocumentIDType marks an input the workflow never should have
	// built.
	invalidDocumentIDType = "invalid_document_id"
)

// Activities holds the use cases the ingest workflow calls. Activities are
// the boundary for this package: they log failures once and turn a failure
// that retrying cannot fix into a non-retryable Temporal error (ADR-0007).
type Activities struct {
	ingest interfaces.IngestService
}

// NewActivities returns the ingest activities on service.
func NewActivities(service interfaces.IngestService) *Activities {
	return &Activities{ingest: service}
}

// ParseAndChunk parses a Document's Source File, stores its Chunks and Parent
// Chunks, and returns how many Chunks it stored, so the workflow can divide
// them into embedding batches. A Source File that cannot be parsed becomes a
// non-retryable application error carrying a reason safe to store, as does a
// Document that is gone; a storage or database failure is returned as-is, so
// Temporal retries it.
func (a *Activities) ParseAndChunk(ctx context.Context, documentID string) (int, error) {
	id, err := parseDocumentID(documentID)
	if err != nil {
		return 0, err
	}
	result, err := a.ingest.ParseAndChunk(ctx, id)
	if err != nil {
		var unparseable *types.UnparseableError
		if errors.As(err, &unparseable) {
			// The cause can name the file, which no log may carry; the reason
			// is the safe part. It is also all the workflow history gets.
			logger.Ctx(ctx).Warn("ingest: could not parse Source File",
				zap.Stringer("document", id), zap.String("reason", unparseable.Reason))
			return 0, temporal.NewNonRetryableApplicationError(unparseable.Reason, unparseableDocumentType, nil)
		}
		var missing *types.DocumentNotFoundError
		if errors.As(err, &missing) {
			logger.Ctx(ctx).Warn("ingest: Document does not exist", zap.Stringer("document", id))
			return 0, temporal.NewNonRetryableApplicationError(missing.Error(), documentNotFoundType, nil)
		}
		attempt := activity.GetInfo(ctx).Attempt
		fields := []zap.Field{zap.Stringer("document", id), zap.Int32("attempt", attempt), zap.Error(err)}
		if attempt < activityAttempts {
			logger.Ctx(ctx).Warn("ingest: parse and chunk failed; retrying", fields...)
		} else {
			logger.Ctx(ctx).Error("ingest: parse and chunk failed", fields...)
		}
		return 0, err
	}
	rejected := make([]string, len(result.Rejected))
	for i, rejection := range result.Rejected {
		rejected[i] = rejection.Tier + ": " + rejection.Reason
	}
	logger.Ctx(ctx).Info("ingest: Chunks stored",
		zap.Stringer("document", id),
		zap.Int("chunks", result.Chunks),
		zap.Int("parent_chunks", result.ParentChunks),
		zap.String("tier", result.Tier),
		zap.Strings("tier_chain", result.TierChain),
		zap.Strings("rejected_tiers", rejected))
	return result.Chunks, nil
}

// EmbedBatch embeds the Chunks of one seq range that have no embedding yet,
// in one model call, and stores them. An unconfigured embedding model becomes
// a non-retryable application error carrying a reason safe to store; a
// provider or database failure is returned as-is, so Temporal retries this
// batch alone without re-running ParseAndChunk or the other batches.
func (a *Activities) EmbedBatch(ctx context.Context, documentID string, from, to int) (int, error) {
	id, err := parseDocumentID(documentID)
	if err != nil {
		return 0, err
	}
	chunks, err := a.ingest.EmbedBatch(ctx, id, from, to)
	if err != nil {
		var unconfigured *types.EmbeddingUnconfiguredError
		if errors.As(err, &unconfigured) {
			logger.Ctx(ctx).Warn("ingest: no embedding model configured",
				zap.Stringer("document", id), zap.Int("from", from), zap.Int("to", to),
				zap.String("reason", unconfigured.Error()))
			return 0, temporal.NewNonRetryableApplicationError(unconfigured.Error(), embeddingUnconfiguredType, nil)
		}
		attempt := activity.GetInfo(ctx).Attempt
		// The error can carry the provider's error body, which vendors
		// sometimes fill with the request, and that body holds Chunk text. Only
		// metadata goes to the log; the model-call line already records the
		// attempt and status.
		fields := []zap.Field{
			zap.Stringer("document", id), zap.Int("from", from), zap.Int("to", to), zap.Int32("attempt", attempt),
		}
		if status := api.HTTPStatusCode(err); status != 0 {
			fields = append(fields, zap.Int("http_status", status))
		}
		if attempt < activityAttempts {
			logger.Ctx(ctx).Warn("ingest: Chunk batch embed failed; retrying", fields...)
		} else {
			logger.Ctx(ctx).Error("ingest: Chunk batch embed failed", fields...)
		}
		return 0, err
	}
	logger.Ctx(ctx).Info("ingest: Chunk batch embedded",
		zap.Stringer("document", id), zap.Int("from", from), zap.Int("to", to), zap.Int("chunks", chunks))
	return chunks, nil
}

// Complete marks a Document completed when every Chunk has an embedding. A
// Document that is gone becomes a non-retryable application error, so the
// workflow does not try to mark a row that no longer exists; a missing
// embedding is returned as-is, so Temporal retries until every batch has
// landed.
func (a *Activities) Complete(ctx context.Context, documentID string) error {
	id, err := parseDocumentID(documentID)
	if err != nil {
		return err
	}
	if err := a.ingest.Complete(ctx, id); err != nil {
		var missing *types.DocumentNotFoundError
		if errors.As(err, &missing) {
			logger.Ctx(ctx).Warn("ingest: Document does not exist", zap.Stringer("document", id))
			return temporal.NewNonRetryableApplicationError(missing.Error(), documentNotFoundType, nil)
		}
		attempt := activity.GetInfo(ctx).Attempt
		fields := []zap.Field{zap.Stringer("document", id), zap.Int32("attempt", attempt), zap.Error(err)}
		if attempt < activityAttempts {
			logger.Ctx(ctx).Warn("ingest: completing Document failed; retrying", fields...)
		} else {
			logger.Ctx(ctx).Error("ingest: completing Document failed", fields...)
		}
		return err
	}
	logger.Ctx(ctx).Info("ingest: Document completed", zap.Stringer("document", id))
	return nil
}

// MarkFailed records that a Document's ingest failed, with a short reason.
func (a *Activities) MarkFailed(ctx context.Context, documentID, reason string) error {
	id, err := parseDocumentID(documentID)
	if err != nil {
		return err
	}
	if err := a.ingest.MarkFailed(ctx, id, reason); err != nil {
		logger.Ctx(ctx).Error("ingest: could not mark Document failed",
			zap.Stringer("document", id), zap.Error(err))
		return err
	}
	return nil
}

// parseDocumentID parses the workflow's only input. A malformed ID can never
// become valid on a retry.
func parseDocumentID(documentID string) (uuid.UUID, error) {
	id, err := uuid.Parse(documentID)
	if err != nil {
		return uuid.Nil, temporal.NewNonRetryableApplicationError("invalid Document ID", invalidDocumentIDType, err)
	}
	return id, nil
}
