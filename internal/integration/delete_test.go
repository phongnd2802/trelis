//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
)

// deleteDocument sends DELETE /documents/{id} with key as the bearer token.
func (tr *trelis) deleteDocument(t *testing.T, key, id string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, tr.url+"/documents/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /documents/%s: %v", id, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// waitForDocumentStatus polls GET /documents/{id} until it reports status.
func (tr *trelis) waitForDocumentStatus(t *testing.T, key, id, status string) {
	t.Helper()
	const timeout = 30 * time.Second
	deadline := time.Now().Add(timeout)
	for {
		resp := tr.get(t, "/documents/"+id, key)
		var out struct {
			Data documentView `json:"data"`
		}
		err := json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /documents/%s = %d (%v), want 200 with the Document", id, resp.StatusCode, err)
		}
		if out.Data.ParseStatus == status {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Document %s still %q after %v, want %q", id, out.Data.ParseStatus, timeout, status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForObjectsGone polls the bucket until no object is under the Document's
// prefix.
func (tr *trelis) waitForObjectsGone(t *testing.T, documentID string) {
	t.Helper()
	prefix := "documents/" + documentID + "/"
	const timeout = 30 * time.Second
	deadline := time.Now().Add(timeout)
	for {
		var remaining []string
		for _, key := range tr.objects(t) {
			if strings.HasPrefix(key, prefix) {
				remaining = append(remaining, key)
			}
		}
		if len(remaining) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bucket still holds %v after %v, want the Document's objects gone", remaining, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// documentUploadID reads the Document's current upload ID from Postgres. A
// test needs it to name the Document's ingest workflow.
func (tr *trelis) documentUploadID(t *testing.T, documentID string) string {
	t.Helper()
	var uploadID string
	if err := tr.pgConn(t).QueryRow(context.Background(),
		"SELECT upload_id FROM documents WHERE id = $1", documentID).Scan(&uploadID); err != nil {
		t.Fatalf("read upload of Document %s: %v", documentID, err)
	}
	return uploadID
}

// ingestWorkflowID is the ID of the ingest workflow for one upload.
func ingestWorkflowID(documentID, uploadID string) string {
	return "ingest/" + documentID + "/" + uploadID
}

// waitForIngestStatus polls Temporal until the upload's ingest workflow has
// status.
func waitForIngestStatus(t *testing.T, documentID, uploadID string, want enumspb.WorkflowExecutionStatus) {
	t.Helper()
	c := temporalClient(t)
	const timeout = 30 * time.Second
	deadline := time.Now().Add(timeout)
	var last string
	for {
		desc, err := c.DescribeWorkflow(context.Background(), ingestWorkflowID(documentID, uploadID), "")
		if err == nil {
			status := desc.Status
			if status == want {
				return
			}
			last = status.String()
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("ingest workflow of Document %s upload %s is %s after %v, want %s",
				documentID, uploadID, last, timeout, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDeletingACompletedDocumentRemovesItsChunksAndObjects(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	id := tr.upload(t, alice, "luat.txt", "# Điều 1\n\nPhạm vi điều chỉnh\n")
	if doc := tr.waitForIngest(t, alice, id); doc.ParseStatus != "completed" {
		t.Fatalf("parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}
	if chunks := tr.chunkContents(t, id); len(chunks) == 0 {
		t.Fatal("no Chunks stored before the delete")
	}
	// A second object under another upload of the same Document, so the
	// cleanup has to remove the whole documents/{id}/ prefix.
	figureKey := "documents/" + id + "/" + uuid.NewString() + "/figures/1.png"
	tr.putObject(t, figureKey, []byte("figure"))

	resp := tr.deleteDocument(t, alice, id)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /documents/%s = %d, want 204", id, resp.StatusCode)
	}
	if resp := tr.get(t, "/documents/"+id, alice); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET the deleted Document = %d, want 404", resp.StatusCode)
	}
	if chunks := tr.chunkContents(t, id); len(chunks) != 0 {
		t.Errorf("Chunks after the delete = %q, want none", chunks)
	}
	tr.waitForObjectsGone(t, id)
	if tr.logContains("document delete: could not cancel the ingest workflow") {
		t.Error("Trelis warned that it could not cancel the completed ingest, want the closed run ignored as unknown")
	}
}

func TestDeletingADocumentMidIngestCancelsItAndStaysGone(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	// Hold the embedding call so the delete lands while the embed batch is in
	// flight. The workflow-side future resolves on cancellation; the activity
	// does not heartbeat, so the worker never delivers the cancel to the
	// request it is waiting on.
	tr.provider.Delay(modeltest.Embeddings, 3*time.Second)
	var content strings.Builder
	for i := range 40 {
		fmt.Fprintf(&content, "## Điều %d\n\nNội dung điều %d.\n\n", i+1, i+1)
	}
	id := tr.upload(t, alice, "luat.txt", content.String())

	tr.waitForDocumentStatus(t, alice, id, "processing")
	deadline := time.Now().Add(30 * time.Second)
	for tr.provider.CallCount(modeltest.Embeddings) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no embeddings call arrived after 30s, so ingest never reached the embed batches")
		}
		time.Sleep(20 * time.Millisecond)
	}

	uploadID := tr.documentUploadID(t, id)
	resp := tr.deleteDocument(t, alice, id)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE mid-ingest = %d, want 204", resp.StatusCode)
	}
	if resp := tr.get(t, "/documents/"+id, alice); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET the deleted Document = %d, want it gone", resp.StatusCode)
	}
	if chunks := tr.chunkContents(t, id); len(chunks) != 0 {
		t.Errorf("Chunks after the delete = %q, want none", chunks)
	}
	tr.waitForObjectsGone(t, id)
	waitForIngestStatus(t, id, uploadID, enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED)
}

func TestAReaderWhoIsNotTheOwnerCannotDelete(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	bob := tr.principalKey(t, "bob")
	id := tr.uploadBytesWithFields(t, alice, "shared.txt", []byte("# Shared\n\nReadable by bob.\n"),
		map[string]string{"principals": "bob"})

	if resp := tr.deleteDocument(t, bob, id); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bob DELETE = %d, want 403 even though he can read it", resp.StatusCode)
	}
	if resp := tr.get(t, "/documents/"+id, alice); resp.StatusCode != http.StatusOK {
		t.Errorf("alice GET after bob's forbidden delete = %d, want 200", resp.StatusCode)
	}
	if resp := tr.deleteDocument(t, alice, id); resp.StatusCode != http.StatusNoContent {
		t.Errorf("alice DELETE = %d, want 204", resp.StatusCode)
	}
}
