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
	// supersededUploadType marks an upload a re-upload replaced while an
	// activity was working for it. There is no row of that upload left to
	// mark, and the new upload must not be marked failed.
	supersededUploadType = "superseded_upload"
	// invalidDocumentIDType and invalidUploadIDType mark an input the
	// workflow never should have built.
	invalidDocumentIDType = "invalid_document_id"
	invalidUploadIDType   = "invalid_upload_id"
)

// Activities holds the use cases the ingest workflow calls. Activities are
// the boundary for this package: they log failures once and turn a failure
// that retrying cannot fix into a non-retryable Temporal error (ADR-0007).
type Activities struct {
	ingest  interfaces.IngestService
	cleanup interfaces.DocumentCleanupService
}

// NewActivities returns the ingest and cleanup activities on their services.
func NewActivities(ingest interfaces.IngestService, cleanup interfaces.DocumentCleanupService) *Activities {
	return &Activities{ingest: ingest, cleanup: cleanup}
}

// ParseAndChunk parses one upload of a Document's Source File, stores its
// Chunks and Parent Chunks, and returns how many Chunks it stored, so the
// workflow can divide them into embedding batches. A Source File that cannot
// be parsed becomes a non-retryable application error carrying a reason safe
// to store, as does a Document that is gone or an upload a re-upload
// superseded; a storage or database failure is returned as-is, so Temporal
// retries it.
func (a *Activities) ParseAndChunk(ctx context.Context, documentID, uploadID string) (int, error) {
	id, err := parseDocumentID(documentID)
	if err != nil {
		return 0, err
	}
	upload, err := parseUploadID(uploadID)
	if err != nil {
		return 0, err
	}
	attempt := activity.GetInfo(ctx).Attempt
	result, err := a.ingest.ParseAndChunk(ctx, id, upload, int(attempt))
	if err != nil {
		var unparseable *types.UnparseableError
		if errors.As(err, &unparseable) {
			// The cause can name the file, which no log may carry; the reason
			// is the safe part. It is also all the workflow history gets.
			logger.Ctx(ctx).Warn("ingest: could not parse Source File",
				zap.Stringer("document", id), zap.String("reason", unparseable.Reason))
			return 0, temporal.NewNonRetryableApplicationError(unparseable.Reason, unparseableDocumentType, nil)
		}
		if mapped := nonRetryableIngestError(err); mapped != nil {
			logger.Ctx(ctx).Warn("ingest: activity cannot retry",
				zap.Stringer("document", id), zap.Stringer("upload", upload), zap.Error(err))
			return 0, mapped
		}
		fields := []zap.Field{zap.Stringer("document", id), zap.Int32("attempt", attempt), zap.Error(err)}
		logActivityFailure(ctx, attempt, activityAttempts, "ingest: parse and chunk failed", fields)
		return 0, err
	}
	rejected := make([]string, len(result.Rejected))
	for i, rejection := range result.Rejected {
		rejected[i] = rejection.Tier + ": " + rejection.Reason
	}
	logger.Ctx(ctx).Info("ingest: Chunks stored",
		zap.Stringer("document", id),
		zap.Stringer("upload", upload),
		zap.Int("chunks", result.Chunks),
		zap.Int("parent_chunks", result.ParentChunks),
		zap.String("tier", result.Tier),
		zap.Strings("tier_chain", result.TierChain),
		zap.Strings("rejected_tiers", rejected))
	return result.Chunks, nil
}

// EmbedBatch embeds the Chunks of one seq range that have no embedding yet,
// in one model call, and stores them. An unconfigured embedding model becomes
// a non-retryable application error carrying a reason safe to store, as does
// an upload a re-upload superseded; a provider or database failure is
// returned as-is, so Temporal retries this batch alone without re-running
// ParseAndChunk or the other batches.
func (a *Activities) EmbedBatch(ctx context.Context, documentID, uploadID string, from, to int) (int, error) {
	id, err := parseDocumentID(documentID)
	if err != nil {
		return 0, err
	}
	upload, err := parseUploadID(uploadID)
	if err != nil {
		return 0, err
	}
	chunks, err := a.ingest.EmbedBatch(ctx, id, upload, from, to)
	if err != nil {
		var unconfigured *types.EmbeddingUnconfiguredError
		if errors.As(err, &unconfigured) {
			logger.Ctx(ctx).Warn("ingest: no embedding model configured",
				zap.Stringer("document", id), zap.Int("from", from), zap.Int("to", to),
				zap.String("reason", unconfigured.Error()))
			return 0, temporal.NewNonRetryableApplicationError(unconfigured.Error(), embeddingUnconfiguredType, nil)
		}
		if mapped := nonRetryableIngestError(err); mapped != nil {
			logger.Ctx(ctx).Warn("ingest: activity cannot retry",
				zap.Stringer("document", id), zap.Stringer("upload", upload),
				zap.Int("from", from), zap.Int("to", to), zap.Error(err))
			return 0, mapped
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
		logActivityFailure(ctx, attempt, activityAttempts, "ingest: Chunk batch embed failed", fields)
		return 0, err
	}
	logger.Ctx(ctx).Info("ingest: Chunk batch embedded",
		zap.Stringer("document", id), zap.Stringer("upload", upload),
		zap.Int("from", from), zap.Int("to", to), zap.Int("chunks", chunks))
	return chunks, nil
}

// Complete marks one upload of a Document completed when every Chunk has an
// embedding. A Document that is gone, or an upload a re-upload superseded,
// becomes a non-retryable application error, so the workflow does not try to
// mark a row that no longer belongs to it; a missing embedding is returned
// as-is, so Temporal retries until every batch has landed.
func (a *Activities) Complete(ctx context.Context, documentID, uploadID string) error {
	id, err := parseDocumentID(documentID)
	if err != nil {
		return err
	}
	upload, err := parseUploadID(uploadID)
	if err != nil {
		return err
	}
	if err := a.ingest.Complete(ctx, id, upload); err != nil {
		if mapped := nonRetryableIngestError(err); mapped != nil {
			logger.Ctx(ctx).Warn("ingest: activity cannot retry",
				zap.Stringer("document", id), zap.Stringer("upload", upload), zap.Error(err))
			return mapped
		}
		attempt := activity.GetInfo(ctx).Attempt
		fields := []zap.Field{zap.Stringer("document", id), zap.Int32("attempt", attempt), zap.Error(err)}
		logActivityFailure(ctx, attempt, activityAttempts, "ingest: completing Document failed", fields)
		return err
	}
	logger.Ctx(ctx).Info("ingest: Document completed",
		zap.Stringer("document", id), zap.Stringer("upload", upload))
	return nil
}

// MarkFailed records that one upload of a Document's ingest failed, with a
// short reason. A superseded upload is a non-retryable application error, so
// the run never marks the new upload failed.
func (a *Activities) MarkFailed(ctx context.Context, documentID, uploadID, reason string) error {
	id, err := parseDocumentID(documentID)
	if err != nil {
		return err
	}
	upload, err := parseUploadID(uploadID)
	if err != nil {
		return err
	}
	if err := a.ingest.MarkFailed(ctx, id, upload, reason); err != nil {
		if mapped := nonRetryableIngestError(err); mapped != nil {
			logger.Ctx(ctx).Warn("ingest: activity cannot retry",
				zap.Stringer("document", id), zap.Stringer("upload", upload), zap.Error(err))
			return mapped
		}
		logger.Ctx(ctx).Error("ingest: could not mark Document failed",
			zap.Stringer("document", id), zap.Error(err))
		return err
	}
	return nil
}

// DeleteSupersededUploads removes the objects of a Document's earlier uploads
// once the current upload's Chunks are committed. An upload a re-upload
// superseded becomes a non-retryable application error; a storage failure is
// returned as-is, so Temporal retries the idempotent cleanup.
func (a *Activities) DeleteSupersededUploads(ctx context.Context, documentID, uploadID string) error {
	id, err := parseDocumentID(documentID)
	if err != nil {
		return err
	}
	upload, err := parseUploadID(uploadID)
	if err != nil {
		return err
	}
	if err := a.ingest.DeleteSupersededUploads(ctx, id, upload); err != nil {
		if mapped := nonRetryableIngestError(err); mapped != nil {
			logger.Ctx(ctx).Warn("ingest: activity cannot retry",
				zap.Stringer("document", id), zap.Stringer("upload", upload), zap.Error(err))
			return mapped
		}
		attempt := activity.GetInfo(ctx).Attempt
		fields := []zap.Field{zap.Stringer("document", id), zap.Stringer("upload", upload),
			zap.Int32("attempt", attempt), zap.Error(err)}
		logActivityFailure(ctx, attempt, deleteSupersededUploadsAttempts,
			"ingest: deleting superseded uploads failed", fields)
		return err
	}
	return nil
}

// DeleteObjects removes every object under a deleted Document's prefix. It is
// idempotent: an empty prefix is not an error, so a retry or a second cleanup
// run is harmless. A storage failure is returned as-is, so Temporal retries
// the activity.
func (a *Activities) DeleteObjects(ctx context.Context, documentID string) error {
	id, err := parseDocumentID(documentID)
	if err != nil {
		return err
	}
	if err := a.cleanup.DeleteDocumentObjects(ctx, id); err != nil {
		attempt := activity.GetInfo(ctx).Attempt
		fields := []zap.Field{zap.Stringer("document", id), zap.Int32("attempt", attempt), zap.Error(err)}
		logActivityFailure(ctx, attempt, deleteObjectsAttempts, "document delete: removing objects failed", fields)
		return err
	}
	logger.Ctx(ctx).Info("document delete: objects removed", zap.Stringer("document", id))
	return nil
}

// logActivityFailure logs one failed attempt: WARN while attempts remain, so
// a retry is expected, and ERROR on the last one, which an operator must look
// at. fields carry IDs and metadata only.
func logActivityFailure(ctx context.Context, attempt int32, maxAttempts int, message string, fields []zap.Field) {
	if int(attempt) < maxAttempts {
		logger.Ctx(ctx).Warn(message+"; retrying", fields...)
	} else {
		logger.Ctx(ctx).Error(message, fields...)
	}
}

// nonRetryableIngestError maps the domain errors that retrying cannot fix to
// Temporal application errors, or returns nil for any other error. Both
// errors' text is safe to keep in workflow history.
func nonRetryableIngestError(err error) error {
	var missing *types.DocumentNotFoundError
	if errors.As(err, &missing) {
		return temporal.NewNonRetryableApplicationError(missing.Error(), documentNotFoundType, nil)
	}
	var superseded *types.SupersededUploadError
	if errors.As(err, &superseded) {
		return temporal.NewNonRetryableApplicationError(superseded.Error(), supersededUploadType, nil)
	}
	return nil
}

// parseDocumentID parses the workflow's Document ID. A malformed ID can never
// become valid on a retry.
func parseDocumentID(documentID string) (uuid.UUID, error) {
	return parseUUID(documentID, "invalid Document ID", invalidDocumentIDType)
}

// parseUploadID parses the workflow's upload ID. A malformed ID can never
// become valid on a retry.
func parseUploadID(uploadID string) (uuid.UUID, error) {
	return parseUUID(uploadID, "invalid upload ID", invalidUploadIDType)
}

// parseUUID parses one workflow input ID. A malformed ID can never become
// valid on a retry.
func parseUUID(value, message, errorType string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, temporal.NewNonRetryableApplicationError(message, errorType, err)
	}
	return id, nil
}
