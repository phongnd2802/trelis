package ingest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.temporal.io/sdk/testsuite"
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
	result   types.ChunkingResult
	attempt  int
	uploadID uuid.UUID
}

func (f *fakeIngestService) ParseAndChunk(_ context.Context, _ uuid.UUID, uploadID uuid.UUID, attempt int) (types.ChunkingResult, error) {
	f.attempt = attempt
	f.uploadID = uploadID
	return f.result, nil
}

func TestParseAndChunkActivityLogsTheChunkingReport(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	documentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000d1")
	uploadID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000e1")
	result := types.ChunkingResult{
		Chunks:       7,
		ParentChunks: 2,
		Tier:         string(chunker.TierHeuristic),
		TierChain:    []string{string(chunker.TierHeuristic), string(chunker.TierRecursive)},
		Rejected: []types.ChunkingRejection{
			{Tier: string(chunker.TierHeading), Reason: "chunk 0 at [0,12) cuts a protected span"},
		},
	}
	service := &fakeIngestService{result: result}
	activity := NewActivities(service, nil)

	// The activity environment supplies the activity context ParseAndChunk reads
	// its attempt from, which a direct call does not.
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activity.ParseAndChunk)
	val, err := env.ExecuteActivity(activity.ParseAndChunk, documentID.String(), uploadID.String())
	if err != nil {
		t.Fatalf("ParseAndChunk: %v", err)
	}
	var got int
	if err := val.Get(&got); err != nil {
		t.Fatalf("read ParseAndChunk result: %v", err)
	}
	if got != result.Chunks {
		t.Errorf("ParseAndChunk = %d, want the stored Chunk count %d", got, result.Chunks)
	}
	if service.attempt != 1 {
		t.Errorf("service got attempt %d, want the activity's first attempt 1", service.attempt)
	}
	if service.uploadID != uploadID {
		t.Errorf("service got upload %s, want %s", service.uploadID, uploadID)
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
	if fmt.Sprint(fields["upload"]) != uploadID.String() {
		t.Errorf("upload = %v, want %s", fields["upload"], uploadID)
	}
	// The line carries IDs, counts and fixed validator reasons only. The
	// activity never receives Chunk text, so no content field or value can
	// appear; assert the shape rather than trusting the caller.
	if len(fields) != len(want)+2 {
		t.Errorf("log fields = %v, want document and upload plus %d report fields", fields, len(want))
	}
	if line := fmt.Sprint(entry); strings.Contains(line, "context_header") || strings.Contains(line, "content") {
		t.Errorf("the Chunks stored line carries Chunk content: %s", line)
	}
}

// fakeCleanupService records the Documents whose objects it was asked to
// delete.
type fakeCleanupService struct {
	interfaces.DocumentCleanupService
	deleted []uuid.UUID
}

func (f *fakeCleanupService) DeleteDocumentObjects(_ context.Context, documentID uuid.UUID) error {
	f.deleted = append(f.deleted, documentID)
	return nil
}

func TestDeleteObjectsActivityCallsTheCleanupServiceWithTheDocumentID(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := logger.WithContext(context.Background(), zap.New(core))
	documentID := uuid.MustParse("0190d2a8-0000-7000-8000-0000000000d2")
	cleanup := &fakeCleanupService{}
	activity := NewActivities(nil, cleanup)

	if err := activity.DeleteObjects(ctx, documentID.String()); err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if !slices.Equal(cleanup.deleted, []uuid.UUID{documentID}) {
		t.Errorf("DeleteDocumentObjects called with %v, want once with %s", cleanup.deleted, documentID)
	}
	entries := logs.FilterMessage("document delete: objects removed").All()
	if len(entries) != 1 {
		t.Fatalf("logger saw %d objects removed lines, want one: %v", len(entries), logs.All())
	}
	if got := entries[0].ContextMap()["document"]; got != documentID.String() {
		t.Errorf("document = %v, want %s", got, documentID)
	}
}
