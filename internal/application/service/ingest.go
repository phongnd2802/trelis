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
// repositories, the object store and a parser. It owns the transaction that
// commits a Document's Chunks together with its status (ADR-0007).
type ingestService struct {
	store     db.Store
	documents interfaces.DocumentRepository
	chunks    interfaces.ChunkRepository
	files     interfaces.FileService
	parser    interfaces.DocumentParser
}

// NewIngestService returns the use cases the ingest activities run.
func NewIngestService(store db.Store, documents interfaces.DocumentRepository,
	chunks interfaces.ChunkRepository, files interfaces.FileService, parser interfaces.DocumentParser,
) interfaces.IngestService {
	return &ingestService{store: store, documents: documents, chunks: chunks, files: files, parser: parser}
}

// ParseAndChunk parses a Document's Source File, splits the Markdown into
// Chunks and, in one transaction, replaces the Document's Chunks and marks it
// completed, returning how many Chunks it stored. A *types.UnparseableError
// means the Source File cannot be turned into text; a
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
		if err := s.chunks.ReplaceChunks(ctx, documentID, chunks); err != nil {
			return err
		}
		return s.documents.SetParseStatus(ctx, documentID, types.ParseStatusCompleted, "")
	})
	if err != nil {
		return 0, err
	}
	return len(chunks), nil
}

// MarkFailed records that a Document's ingest failed, with a reason short
// enough to show beside the Document.
func (s *ingestService) MarkFailed(ctx context.Context, documentID uuid.UUID, reason string) error {
	return s.documents.SetParseStatus(ctx, documentID, types.ParseStatusFailed, reason)
}
