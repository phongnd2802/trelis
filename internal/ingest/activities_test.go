package ingest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/phongnd2802/trelis/internal/chunker"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// fakeIngestService answers ParseAndChunk with one fixed result; the other
// methods are never called here, so the embedded interface can stay nil.
type fakeIngestService struct {
	interfaces.IngestService
	result types.ChunkingResult
}

func (f *fakeIngestService) ParseAndChunk(context.Context, uuid.UUID) (types.ChunkingResult, error) {
	return f.result, nil
}

func TestParseAndChunkActivityLogsTheChunkingReport(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := logger.WithContext(context.Background(), zap.New(core))
	documentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000d1")
	result := types.ChunkingResult{
		Chunks:       7,
		ParentChunks: 2,
		Tier:         string(chunker.TierHeuristic),
		TierChain:    []string{string(chunker.TierHeuristic), string(chunker.TierRecursive)},
		Rejected: []types.ChunkingRejection{
			{Tier: string(chunker.TierHeading), Reason: "chunk 0 at [0,12) cuts a protected span"},
		},
	}
	activity := NewActivities(&fakeIngestService{result: result})

	got, err := activity.ParseAndChunk(ctx, documentID.String())
	if err != nil {
		t.Fatalf("ParseAndChunk: %v", err)
	}
	if got != result.Chunks {
		t.Errorf("ParseAndChunk = %d, want the stored Chunk count %d", got, result.Chunks)
	}

	entries := logs.FilterMessage("ingest: Chunks stored").All()
	if len(entries) != 1 {
		t.Fatalf("logger saw %d Chunks stored lines, want one per Document: %v", len(entries), logs.All())
	}
	entry := entries[0]
	if entry.Level != zapcore.InfoLevel {
		t.Errorf("level = %v, want INFO", entry.Level)
	}
	fields := entry.ContextMap()
	want := map[string]any{
		"chunks":         int64(7),
		"parent_chunks":  int64(2),
		"tier":           string(chunker.TierHeuristic),
		"tier_chain":     []string{string(chunker.TierHeuristic), string(chunker.TierRecursive)},
		"rejected_tiers": []string{"heading: chunk 0 at [0,12) cuts a protected span"},
	}
	for key, value := range want {
		if fmt.Sprint(fields[key]) != fmt.Sprint(value) {
			t.Errorf("%s = %v, want %v", key, fields[key], value)
		}
	}
	if fmt.Sprint(fields["document"]) != documentID.String() {
		t.Errorf("document = %v, want %s", fields["document"], documentID)
	}
	// The line carries IDs, counts and fixed validator reasons only. The
	// activity never receives Chunk text, so no content field or value can
	// appear; assert the shape rather than trusting the caller.
	if len(fields) != len(want)+1 {
		t.Errorf("log fields = %v, want document plus %d report fields", fields, len(want))
	}
	if line := fmt.Sprint(entry); strings.Contains(line, "context_header") || strings.Contains(line, "content") {
		t.Errorf("the Chunks stored line carries Chunk content: %s", line)
	}
}
