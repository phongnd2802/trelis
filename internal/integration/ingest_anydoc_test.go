//go:build integration && anydoc

package integration

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"github.com/phongnd2802/trelis/internal/ingest"
)

// TestADocxGivesACompletedDocumentAndChunks is the happy path with the real
// engine: a docx becomes Markdown, the Chunks are stored and the Document is
// completed.
func TestADocxGivesACompletedDocumentAndChunks(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	data, err := os.ReadFile(filepath.Join("..", "parser", "anydoc", "testdata", "upstream", "docx", "text.docx"))
	if err != nil {
		t.Fatal(err)
	}
	id := tr.uploadBytes(t, alice, "text.docx", data)

	doc := tr.waitForIngest(t, alice, id)
	if doc.ParseStatus != "completed" {
		t.Fatalf("parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}
	if doc.ParseError != "" {
		t.Errorf("parse_error = %q, want none on success", doc.ParseError)
	}
	chunks := tr.chunkContents(t, id)
	if len(chunks) == 0 {
		t.Fatal("no Chunks stored for a parsed docx")
	}
	for i, content := range chunks {
		if strings.TrimSpace(content) == "" {
			t.Errorf("chunk %d is blank, want its text", i)
		}
	}
}

// TestStartingIngestTwiceReusesTheCompletedRun proves the workflow ID really
// makes a repeat start harmless: the completed run is not followed by a new
// one, and the Chunks stay byte-for-byte the same.
func TestStartingIngestTwiceReusesTheCompletedRun(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	data, err := os.ReadFile(filepath.Join("..", "parser", "anydoc", "testdata", "upstream", "docx", "text.docx"))
	if err != nil {
		t.Fatal(err)
	}
	id := tr.uploadBytes(t, alice, "text.docx", data)
	if doc := tr.waitForIngest(t, alice, id); doc.ParseStatus != "completed" {
		t.Fatalf("parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}
	before := tr.chunkContents(t, id)
	if len(before) == 0 {
		t.Fatal("no Chunks stored for a parsed docx")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := client.Dial(client.Options{HostPort: net.JoinHostPort(deps.temporal.host, deps.temporal.port)})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer c.Close()
	runID := workflowRunID(t, c, id)

	starter := ingest.NewStarter(c, tr.taskQueue)
	documentID := uuid.MustParse(id)
	for range 2 {
		if err := starter.StartIngest(ctx, documentID); err != nil {
			t.Fatalf("StartIngest: %v", err)
		}
	}

	if after := workflowRunID(t, c, id); after != runID {
		t.Errorf("workflow run changed from %s to %s after starting twice, want the same run", runID, after)
	}
	if doc := tr.waitForIngest(t, alice, id); doc.ParseStatus != "completed" {
		t.Errorf("parse_status = %q after starting twice, want completed", doc.ParseStatus)
	}
	if after := tr.chunkContents(t, id); !slices.Equal(after, before) {
		t.Errorf("Chunks changed after starting twice:\nbefore %q\nafter  %q", before, after)
	}
}

// workflowRunID is the workflow run Temporal currently serves documentID from.
func workflowRunID(t *testing.T, c client.Client, documentID string) string {
	t.Helper()
	desc, err := c.DescribeWorkflow(context.Background(), documentID, "")
	if err != nil {
		t.Fatalf("describe workflow %s: %v", documentID, err)
	}
	return desc.WorkflowExecution.RunID
}
