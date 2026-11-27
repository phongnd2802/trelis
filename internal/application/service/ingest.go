package service

import (
	"context"
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
	files     interfaces.FileService
	parser    interfaces.DocumentParser
	// embedder is nil when models.embedding is not configured, which makes
	// EmbedBatch fail with a *types.EmbeddingUnconfiguredError.
	embedder interfaces.Embedder
}

// NewIngestService returns the use cases the ingest activities run. embedder
// may be nil when no embedding model is configured.
func NewIngestService(store db.Store, documents interfaces.DocumentRepository,
	chunks interfaces.ChunkRepository, files interfaces.FileService, parser interfaces.DocumentParser,
	embedder interfaces.Embedder,
) interfaces.IngestService {
	return &ingestService{store: store, documents: documents, chunks: chunks, files: files, parser: parser, embedder: embedder}
}

// ParseAndChunk parses a Document's Source File, splits the Markdown into
// Chunks and, in one transaction, replaces the Document's Chunks. It leaves
// the Document processing for the embed batches; Complete marks it completed
// once every Chunk has a vector. It returns how many Chunks it stored. A
// *types.UnparseableError means the Source File cannot be turned into text; a
// *types.DocumentNotFoundError means the row is gone. Neither can be fixed
// by retrying; a storage or database failure is not, so the activity retries
// it.
func (s *ingestService) ParseAndChunk(ctx context.Context, documentID uuid.UUID) (int, error) {
	doc, err := s.documents.GetDocumentByID(ctx, documentID)
	if errors.Is(err, repository.ErrDocumentNotFound) {
		return 0, &types.DocumentNotFoundError{}
	}
	if err != nil {
		return 0, err
	}
	if err := s.documents.SetParseStatus(ctx, documentID, types.ParseStatusProcessing, ""); err != nil {
		if errors.Is(err, repository.ErrDocumentNotFound) {
			return 0, &types.DocumentNotFoundError{}
		}
		return 0, err
	}

	body, err := s.files.GetFile(ctx, doc.ObjectKey)
	if err != nil {
		return 0, fmt.Errorf("open Source File of document %s: %w", documentID, err)
	}
	fileContent, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil {
		return 0, fmt.Errorf("read Source File of document %s: %w", documentID, readErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close Source File of document %s: %w", documentID, closeErr)
	}

	result, err := s.parser.Parse(ctx, &types.ParseRequest{
		FileContent: fileContent,
		FileName:    doc.FileName,
		FileType:    doc.FileType,
		Title:       doc.Title,
	})
	if err != nil {
		return 0, types.NewUnparseableError("the Source File could not be parsed", err)
	}
	pieces := chunker.Split(result.MarkdownContent, chunker.DefaultConfig())
	if len(pieces) == 0 {
		// Split has no Chunks only when the Markdown is blank, which no
		// retry can change.
		return 0, types.NewUnparseableError("the Source File has no extractable text", nil)
	}
	chunks := make([]*types.Chunk, 0, len(pieces))
	for _, piece := range pieces {
		chunks = append(chunks, &types.Chunk{
			DocumentID:    documentID,
			Seq:           piece.Seq,
			Content:       piece.Content,
			ContextHeader: piece.ContextHeader,
			StartOffset:   piece.Start,
			EndOffset:     piece.End,
		})
	}

	err = s.store.ExecTx(ctx, func(ctx context.Context) error {
		return s.chunks.ReplaceChunks(ctx, documentID, chunks)
	})
	if err != nil {
		return 0, err
	}
	return len(chunks), nil
}

// EmbedBatch embeds the Chunks of a Document whose Seq is in [from, to) and
// that have no embedding yet, in one embedding call, then stores their vectors
// in one transaction. It returns how many Chunks it embedded; a range that is
// already embedded is a no-op. The text is the Chunk's ContextHeader, when it
// has one, followed by its Content, which is what a reader of that Chunk sees.
// A *types.EmbeddingUnconfiguredError means models.embedding is unset and
// cannot be fixed by retrying. An embedding provider or database failure is,
// so the activity retries this batch alone without re-parsing the Document.
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
// one transaction that counts the Chunks without one and sets the status. A
// missing embedding is a retryable error naming the count, so the Document is
// never completed with part of its Chunks unembedded. A
// *types.DocumentNotFoundError means the Document row is gone and cannot be
// fixed by retrying.
func (s *ingestService) Complete(ctx context.Context, documentID uuid.UUID) error {
	err := s.store.ExecTx(ctx, func(ctx context.Context) error {
		missing, err := s.chunks.CountChunksWithoutEmbedding(ctx, documentID)
		if err != nil {
			return err
		}
		if missing > 0 {
			return fmt.Errorf("document %s: %d Chunks without an embedding", documentID, missing)
		}
		return s.documents.SetParseStatus(ctx, documentID, types.ParseStatusCompleted, "")
	})
	if errors.Is(err, repository.ErrDocumentNotFound) {
		return &types.DocumentNotFoundError{}
	}
	return err
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
// enough to show beside the Document. A Document deleted since the failure has
// no row left to mark, so that is not an error.
func (s *ingestService) MarkFailed(ctx context.Context, documentID uuid.UUID, reason string) error {
	err := s.documents.SetParseStatus(ctx, documentID, types.ParseStatusFailed, reason)
	if errors.Is(err, repository.ErrDocumentNotFound) {
		return nil
	}
	return err
}
