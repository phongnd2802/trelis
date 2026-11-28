package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/chunker"
	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// ingestService implements IngestService over the Document and Chunk
// repositories, the object store, a parser and an embedding model. It owns
// the transactions that commit a Document's Chunks and their embeddings
// together with its status (ADR-0007).
type ingestService struct {
	store     db.Store
	documents interfaces.DocumentRepository
	chunks    interfaces.ChunkRepository
	events    interfaces.IngestEventRepository
	files     interfaces.FileService
	parser    interfaces.DocumentParser
	// splitter is the tier, sizes and parent-child mode ingest runs.
	splitter chunker.SplitterConfig
	// embedder is nil when models.embedding is not configured, which makes
	// EmbedBatch fail with a *types.EmbeddingUnconfiguredError.
	embedder interfaces.Embedder
}

// NewIngestService returns the use cases the ingest activities run. embedder
// may be nil when no embedding model is configured. splitter carries the
// chunking settings; a zero value splits with the chunker defaults. events
// records the progress a client follows over GET /documents/{id}/events.
func NewIngestService(store db.Store, documents interfaces.DocumentRepository,
	chunks interfaces.ChunkRepository, events interfaces.IngestEventRepository,
	files interfaces.FileService, parser interfaces.DocumentParser,
	embedder interfaces.Embedder, splitter chunker.SplitterConfig,
) interfaces.IngestService {
	return &ingestService{store: store, documents: documents, chunks: chunks, events: events,
		files: files, parser: parser, embedder: embedder, splitter: splitter}
}

// ParseAndChunk parses a Document's Source File, splits the Markdown into
// Chunks and, in one transaction, replaces the Document's Chunks and Parent
// Chunks. It leaves the Document processing for the embed batches; Complete
// marks it completed once every Chunk has a vector. It reports what it stored
// and how the tier chain chose it, and records parse_started for attempt
// before it starts and parse_finished in the transaction that commits the
// Chunks. With parent-child mode off only the Chunks are stored; with it on
// the Parent Chunks are stored as well but never embedded. A
// *types.UnparseableError means the Source File cannot be turned into text; a
// *types.DocumentNotFoundError means the row is gone. Neither can be fixed by
// retrying; a storage or database failure is not, so the activity retries it.
func (s *ingestService) ParseAndChunk(ctx context.Context, documentID uuid.UUID, attempt int) (types.ChunkingResult, error) {
	doc, err := s.documents.GetDocumentByID(ctx, documentID)
	if err != nil {
		return types.ChunkingResult{}, ingestDocumentError(err)
	}
	if err := s.appendEvent(ctx, documentID, types.IngestEventParseStarted,
		types.ParseStartedDetail{Attempt: attempt}); err != nil {
		return types.ChunkingResult{}, err
	}
	if err := s.documents.SetParseStatus(ctx, documentID, types.ParseStatusProcessing, ""); err != nil {
		return types.ChunkingResult{}, ingestDocumentError(err)
	}

	body, err := s.files.GetFile(ctx, doc.ObjectKey)
	if err != nil {
		return types.ChunkingResult{}, fmt.Errorf("open Source File of document %s: %w", documentID, err)
	}
	fileContent, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil {
		return types.ChunkingResult{}, fmt.Errorf("read Source File of document %s: %w", documentID, readErr)
	}
	if closeErr != nil {
		return types.ChunkingResult{}, fmt.Errorf("close Source File of document %s: %w", documentID, closeErr)
	}

	result, err := s.parser.Parse(ctx, &types.ParseRequest{
		FileContent: fileContent,
		FileName:    doc.FileName,
		FileType:    doc.FileType,
		Title:       doc.Title,
	})
	if err != nil {
		return types.ChunkingResult{}, types.NewUnparseableError("the Source File could not be parsed", err)
	}
	split, diag := chunker.SplitParentChildWithDiagnostics(result.MarkdownContent, s.splitter)
	if len(split.Children) == 0 {
		// The splitter has no Chunks only when the Markdown is blank, which no
		// retry can change.
		return types.ChunkingResult{}, types.NewUnparseableError("the Source File has no extractable text", nil)
	}
	withParents := s.splitter.ParentChunkSize > 0
	set := chunkSet(documentID, split, withParents)

	err = s.store.ExecTx(ctx, func(ctx context.Context) error {
		if err := s.appendEvent(ctx, documentID, types.IngestEventParseFinished,
			types.ParseFinishedDetail{Chunks: len(set.Chunks)}); err != nil {
			return err
		}
		return s.chunks.ReplaceChunks(ctx, documentID, set)
	})
	if err != nil {
		return types.ChunkingResult{}, err
	}
	return chunkingResult(split, diag, withParents), nil
}

// chunkSet converts a parent-child split into the Chunk set to store. When
// the mode is off only the Chunks are stored, so a Document ingested without
// it has no Parent Chunk rows.
func chunkSet(documentID uuid.UUID, split chunker.ParentChildResult, parents bool) types.ChunkSet {
	set := types.ChunkSet{Chunks: make([]*types.Chunk, len(split.Children))}
	for i, child := range split.Children {
		set.Chunks[i] = &types.Chunk{
			DocumentID:    documentID,
			Seq:           child.Seq,
			Content:       child.Content,
			ContextHeader: child.ContextHeader,
			StartOffset:   child.Start,
			EndOffset:     child.End,
		}
	}
	if !parents {
		return set
	}
	set.Parents = make([]*types.ParentChunk, len(split.Parents))
	for i, parent := range split.Parents {
		set.Parents[i] = &types.ParentChunk{
			DocumentID:    documentID,
			Seq:           parent.Seq,
			Content:       parent.Content,
			ContextHeader: parent.ContextHeader,
			StartOffset:   parent.Start,
			EndOffset:     parent.End,
		}
	}
	set.ParentOf = make([]int, len(split.Children))
	for i, child := range split.Children {
		set.ParentOf[i] = child.Parent
	}
	return set
}

// chunkingResult is the activity-facing report of a split: the stored
// counts and the tier chain's diagnostics as strings. ParentChunks counts
// only Parent Chunks that were stored, so it is 0 with parent-child mode off.
func chunkingResult(split chunker.ParentChildResult, diag *chunker.Diagnostics, parents bool) types.ChunkingResult {
	result := types.ChunkingResult{
		Chunks:    len(split.Children),
		Tier:      string(diag.SelectedTier),
		TierChain: make([]string, len(diag.TierChain)),
		Rejected:  make([]types.ChunkingRejection, len(diag.Rejected)),
	}
	if parents {
		result.ParentChunks = len(split.Parents)
	}
	for i, tier := range diag.TierChain {
		result.TierChain[i] = string(tier)
	}
	for i, rejection := range diag.Rejected {
		result.Rejected[i] = types.ChunkingRejection{Tier: string(rejection.Tier), Reason: rejection.Reason}
	}
	return result
}

// EmbedBatch embeds the Chunks of a Document whose Seq is in [from, to) and
// that have no embedding yet, in one embedding call, then stores their vectors
// in one transaction with an embed_batch_finished Ingest Event. It returns how
// many Chunks it embedded; a range that is already embedded is a no-op and
// writes no event, because no batch finished embedding anything.
// The text is the Chunk's ContextHeader, when it has one, followed by its
// Content, which is what a reader of that Chunk sees. A
// *types.EmbeddingUnconfiguredError means models.embedding is unset and cannot
// be fixed by retrying. An embedding provider or database failure is, so the
// activity retries this batch alone without re-parsing the Document.
func (s *ingestService) EmbedBatch(ctx context.Context, documentID uuid.UUID, from, to int) (int, error) {
	if s.embedder == nil {
		return 0, &types.EmbeddingUnconfiguredError{}
	}
	chunks, err := s.chunks.ListUnembeddedChunks(ctx, documentID, from, to)
	if err != nil {
		return 0, err
	}
	if len(chunks) == 0 {
		// The range is already embedded, or the Document's Chunks changed
		// under it. Either way there is nothing to do here.
		return 0, nil
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = chunkText(c)
	}
	vectors, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("embed %d Chunks %d-%d of document %s: %w",
			len(chunks), from, to, documentID, err)
	}
	if len(vectors) != len(chunks) {
		return 0, fmt.Errorf("embedding model returned %d vectors for %d Chunks %d-%d of document %s",
			len(vectors), len(chunks), from, to, documentID)
	}

	model := s.embedder.GetModelName()
	err = s.store.ExecTx(ctx, func(ctx context.Context) error {
		if err := s.appendEvent(ctx, documentID, types.IngestEventEmbedBatchFinished,
			types.EmbedBatchFinishedDetail{From: from, To: to, Chunks: len(chunks)}); err != nil {
			return err
		}
		for i, c := range chunks {
			if err := s.chunks.SetChunkEmbedding(ctx, c.ID, vectors[i], model); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(chunks), nil
}

// Complete marks a Document completed once every Chunk has an embedding, in
// one transaction that records the completed Ingest Event, counts the Chunks
// without one and sets the status. A missing embedding is a retryable error
// naming the count, so the Document is never completed with part of its
// Chunks unembedded and the completed event rolls back with it. A
// *types.DocumentNotFoundError means the Document row is gone and cannot be
// fixed by retrying.
func (s *ingestService) Complete(ctx context.Context, documentID uuid.UUID) error {
	err := s.store.ExecTx(ctx, func(ctx context.Context) error {
		if err := s.appendEvent(ctx, documentID, types.IngestEventCompleted, struct{}{}); err != nil {
			return err
		}
		missing, err := s.chunks.CountChunksWithoutEmbedding(ctx, documentID)
		if err != nil {
			return err
		}
		if missing > 0 {
			return fmt.Errorf("document %s: %d Chunks without an embedding", documentID, missing)
		}
		if err := s.documents.SetParseStatus(ctx, documentID, types.ParseStatusCompleted, ""); err != nil {
			return ingestDocumentError(err)
		}
		return nil
	})
	return ingestDocumentError(err)
}

// chunkText is the text a Chunk is embedded from: its ContextHeader, such as
// a heading breadcrumb, above its source text when there is one. The BM25
// index (migrations/00008_bm25_chunks.sql) and SearchBM25Chunks
// (internal/db/queries/chunks.sql) index and search this same expression;
// pg_search only uses the index on an exact match, so all three must change
// together.
func chunkText(c *types.Chunk) string {
	if c.ContextHeader == "" {
		return c.Content
	}
	return c.ContextHeader + "\n\n" + c.Content
}

// MarkFailed records that a Document's ingest failed, with a reason short
// enough to show beside the Document, in one transaction that also records
// the failed Ingest Event. A Document deleted since the failure has no row
// left to mark, so that is not an error and writes no event.
func (s *ingestService) MarkFailed(ctx context.Context, documentID uuid.UUID, reason string) error {
	err := s.store.ExecTx(ctx, func(ctx context.Context) error {
		if err := s.appendEvent(ctx, documentID, types.IngestEventFailed,
			types.FailedDetail{Reason: reason}); err != nil {
			return err
		}
		if err := s.documents.SetParseStatus(ctx, documentID, types.ParseStatusFailed, reason); err != nil {
			return ingestDocumentError(err)
		}
		return nil
	})
	var missing *types.DocumentNotFoundError
	if errors.As(err, &missing) {
		return nil
	}
	return err
}

// ingestDocumentError maps the repository's missing-Document error to the
// domain error the activities treat as non-retryable. Other errors pass
// through, nil included.
func ingestDocumentError(err error) error {
	if errors.Is(err, repository.ErrDocumentNotFound) {
		return &types.DocumentNotFoundError{}
	}
	return err
}

// appendEvent records one Ingest Event in ctx. A caller inside an ExecTx gets
// the event committed or rolled back with the data it describes, so a step
// that claims to have finished never leaves an event when its data does not
// land. It is the first statement of every such transaction: the insert locks
// the Document row before the transaction touches any Chunk rows, the same
// order the delete cascade uses, so the two cannot deadlock. The lock is held
// until commit, which is what orders seq by commit.
func (s *ingestService) appendEvent(ctx context.Context, documentID uuid.UUID, eventType string, detail any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encode %s event of document %s: %w", eventType, documentID, err)
	}
	_, err = s.events.InsertIngestEvent(ctx, &types.IngestEvent{
		DocumentID: documentID,
		Type:       eventType,
		Detail:     raw,
	})
	return ingestDocumentError(err)
}
