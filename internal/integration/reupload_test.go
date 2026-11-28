//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"

	"github.com/phongnd2802/trelis/internal/models/modeltest"
	"github.com/phongnd2802/trelis/internal/types"
)

// replaceDocument PUTs one text file to /documents/{id}/file as key.
func (tr *trelis) replaceDocument(t *testing.T, key, id, name, content string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, tr.url+"/documents/"+id+"/file", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /documents/%s/file: %v", id, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// documentFileView is the part of a Document a replacement test reads: its
// identity, status and current file metadata.
type documentFileView struct {
	ID          string `json:"id"`
	ParseStatus string `json:"parse_status"`
	FileName    string `json:"file_name"`
	FileType    string `json:"file_type"`
	FileSize    int64  `json:"file_size"`
	Title       string `json:"title"`
}

// replaceResponse decodes a 200 replacement answer.
func replaceResponse(t *testing.T, resp *http.Response) documentFileView {
	t.Helper()
	var out struct {
		Data documentFileView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode the replacement answer: %v", err)
	}
	return out.Data
}

// objectUploads returns the upload IDs that have objects under the
// Document's prefix.
func (tr *trelis) objectUploads(t *testing.T, documentID string) map[string][]string {
	t.Helper()
	prefix := "documents/" + documentID + "/"
	uploads := map[string][]string{}
	for _, key := range tr.objects(t) {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		uploadID, _, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		uploads[uploadID] = append(uploads[uploadID], key)
	}
	return uploads
}

// waitForUploadObjectsGone polls the bucket until no object is under one
// upload's prefix.
func (tr *trelis) waitForUploadObjectsGone(t *testing.T, documentID, uploadID string) {
	t.Helper()
	prefix := "documents/" + documentID + "/" + uploadID + "/"
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
			t.Fatalf("bucket still holds %v after %v, want upload %s gone", remaining, timeout, uploadID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForFailedEventCount returns how many failed Ingest Events the Document
// has recorded.
func (tr *trelis) failedEventCount(t *testing.T, documentID string) int {
	t.Helper()
	var count int
	if err := tr.pgConn(t).QueryRow(context.Background(),
		"SELECT count(*) FROM ingest_events WHERE document_id = $1 AND type = $2",
		documentID, types.IngestEventFailed).Scan(&count); err != nil {
		t.Fatalf("count failed events of %s: %v", documentID, err)
	}
	return count
}

func TestReuploadingADocumentReplacesTheOldVersionEverywhere(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	const (
		v1 = "# Version One\n\nuniqueverbalpha is the word of the first version.\n"
		v2 = "# Version Two\n\nuniqueverbbeta is the word of the second version.\n"
	)
	id := tr.upload(t, alice, "v1.txt", v1)
	if doc := tr.waitForIngest(t, alice, id); doc.ParseStatus != "completed" {
		t.Fatalf("v1 parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}
	oldUpload := tr.documentUploadID(t, id)

	resp := tr.replaceDocument(t, alice, id, "v2.txt", v2)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT v2 = %d %s, want 200", resp.StatusCode, body)
	}
	replaced := replaceResponse(t, resp)
	if replaced.ID != id || replaced.ParseStatus != "pending" || replaced.FileName != "v2.txt" ||
		replaced.FileType != "txt" || replaced.FileSize != int64(len(v2)) || replaced.Title != "v2.txt" {
		t.Fatalf("replacement = %+v, want the same Document pending with v2's metadata", replaced)
	}
	if doc := tr.waitForIngest(t, alice, id); doc.ParseStatus != "completed" {
		t.Fatalf("v2 parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}
	newUpload := tr.documentUploadID(t, id)
	if newUpload == oldUpload {
		t.Fatalf("upload ID after the re-upload = %s, want a new one", newUpload)
	}

	// Retrieval cannot reach the old version any more, and does reach the new
	// one. BM25 needs no pinned vectors: the words are unique per version.
	old := tr.retrieve(t, alice, "uniqueverbalpha", "bm25", 10)
	if len(old.Data.Results) != 0 {
		t.Errorf("retrieval found %d Chunks of the old version: %+v", len(old.Data.Results), old.Data.Results)
	}
	current := tr.retrieve(t, alice, "uniqueverbbeta", "bm25", 10)
	if len(current.Data.Results) == 0 {
		t.Fatal("retrieval found no Chunk of the new version")
	}
	for _, result := range current.Data.Results {
		if !strings.Contains(result.Content, "uniqueverbbeta") {
			t.Errorf("retrieved Chunk %q does not come from v2", result.Content)
		}
	}

	chunks := strings.Join(tr.chunkContents(t, id), "\n")
	if !strings.Contains(chunks, "uniqueverbbeta") || strings.Contains(chunks, "uniqueverbalpha") {
		t.Errorf("stored Chunks = %q, want only v2", chunks)
	}
	uploads := tr.objectUploads(t, id)
	if _, ok := uploads[newUpload]; !ok {
		t.Errorf("bucket uploads = %v, want v2's %s", uploads, newUpload)
	}
	if _, ok := uploads[oldUpload]; ok {
		t.Errorf("bucket uploads = %v, still hold v1's %s", uploads, oldUpload)
	}

	// A subscriber that starts from 0 replays v1's events, passes v1's
	// completed event and upload_replaced, and ends at v2's completed.
	events := readIngestEvents(t, tr, alice, id, 0)
	var sawReplaced, sawOldCompleted, sawNewCompleted bool
	for _, event := range events {
		switch {
		case event.Type == types.IngestEventUploadReplaced:
			sawReplaced = true
		case event.Type == types.IngestEventCompleted && event.UploadID == oldUpload:
			sawOldCompleted = true
		case event.Type == types.IngestEventCompleted && event.UploadID == newUpload:
			sawNewCompleted = true
		}
	}
	if !sawReplaced || !sawOldCompleted || !sawNewCompleted {
		t.Fatalf("stream events = %v (replaced %v, old completed %v, new completed %v), want all three",
			eventTypes(events), sawReplaced, sawOldCompleted, sawNewCompleted)
	}
	last := events[len(events)-1]
	if last.Type != types.IngestEventCompleted || last.UploadID != newUpload {
		t.Errorf("stream ended at %s of upload %s, want completed of the current upload %s",
			last.Type, last.UploadID, newUpload)
	}
}

func TestReuploadDuringAnInflightIngestSupersedesItCleanly(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	tr.provider.Delay(modeltest.Embeddings, 3*time.Second)
	var v1 strings.Builder
	v1.WriteString("# Version One\n\nuniqueverbalpha is the word of the first version.\n\n")
	for i := range 40 {
		fmt.Fprintf(&v1, "## Điều %d\n\nNội dung điều %d.\n\n", i+1, i+1)
	}
	const v2 = "# Version Two\n\nuniqueverbbeta is the word of the second version.\n"

	id := tr.upload(t, alice, "v1.txt", v1.String())
	tr.waitForDocumentStatus(t, alice, id, "processing")
	deadline := time.Now().Add(30 * time.Second)
	for tr.provider.CallCount(modeltest.Embeddings) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no embeddings call arrived after 30s, so v1 never reached the embed batches")
		}
		time.Sleep(20 * time.Millisecond)
	}
	oldUpload := tr.documentUploadID(t, id)

	resp := tr.replaceDocument(t, alice, id, "v2.txt", v2)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT v2 during v1's ingest = %d, want 200", resp.StatusCode)
	}
	if doc := tr.waitForIngest(t, alice, id); doc.ParseStatus != "completed" {
		t.Fatalf("v2 parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}
	newUpload := tr.documentUploadID(t, id)
	if newUpload == oldUpload {
		t.Fatalf("upload ID after the re-upload = %s, want a new one", newUpload)
	}

	chunks := strings.Join(tr.chunkContents(t, id), "\n")
	if !strings.Contains(chunks, "uniqueverbbeta") || strings.Contains(chunks, "Nội dung điều") {
		t.Errorf("stored Chunks = %q, want only v2's", chunks)
	}
	if failed := tr.failedEventCount(t, id); failed != 0 {
		t.Errorf("the Document recorded %d failed events, want a re-upload to never fail the new version", failed)
	}
	waitForIngestStatus(t, id, oldUpload, enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED)
	tr.waitForUploadObjectsGone(t, id, oldUpload)
	uploads := tr.objectUploads(t, id)
	if _, ok := uploads[newUpload]; !ok {
		t.Errorf("bucket uploads = %v, want v2's %s kept", uploads, newUpload)
	}
}

func TestOnlyTheOwnerMayReuploadAndTheSameContentIsAConflict(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	bob := tr.principalKey(t, "bob")
	carol := tr.principalKey(t, "carol")
	const v1 = "# Version One\n\nuniqueverbalpha is the word of the first version.\n"
	id := tr.uploadBytesWithFields(t, alice, "v1.txt", []byte(v1), map[string]string{"principals": "bob"})
	if doc := tr.waitForIngest(t, alice, id); doc.ParseStatus != "completed" {
		t.Fatalf("v1 parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}

	if resp := tr.replaceDocument(t, bob, id, "v2.txt", "new content"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bob PUT = %d, want 403 even though he can read it", resp.StatusCode)
	}
	unreadable := tr.replaceDocument(t, carol, id, "v2.txt", "new content")
	missing := tr.replaceDocument(t, carol, uuid.NewString(), "v2.txt", "new content")
	if unreadable.StatusCode != http.StatusNotFound || missing.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT as a non-reader and on a missing Document = %d and %d, want both 404",
			unreadable.StatusCode, missing.StatusCode)
	}
	unreadableBody, missingBody := new(bytes.Buffer), new(bytes.Buffer)
	_, _ = unreadableBody.ReadFrom(unreadable.Body)
	_, _ = missingBody.ReadFrom(missing.Body)
	if unreadableBody.String() != missingBody.String() {
		t.Errorf("unreadable body %s differs from missing body %s, which reveals existence",
			unreadableBody, missingBody)
	}

	same := tr.replaceDocument(t, alice, id, "copy.txt", v1)
	if same.StatusCode != http.StatusConflict {
		t.Fatalf("PUT with the current content = %d, want 409", same.StatusCode)
	}
	var duplicate struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(same.Body).Decode(&duplicate); err != nil {
		t.Fatalf("decode the duplicate answer: %v", err)
	}
	if duplicate.Code != "duplicate_document" || duplicate.Data.ID != id {
		t.Errorf("duplicate answer = %+v, want code duplicate_document and Document %s", duplicate, id)
	}

	const otherContent = "# Another Document\n\nThis content belongs to another Document.\n"
	other := tr.upload(t, alice, "other.txt", otherContent)
	if doc := tr.waitForIngest(t, alice, other); doc.ParseStatus != "completed" {
		t.Fatalf("other parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}
	conflict := tr.replaceDocument(t, alice, id, "copy.txt", otherContent)
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("PUT with another Document's content = %d, want 409", conflict.StatusCode)
	}
	duplicate = struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		Code string `json:"code"`
	}{}
	if err := json.NewDecoder(conflict.Body).Decode(&duplicate); err != nil {
		t.Fatalf("decode the duplicate answer: %v", err)
	}
	if duplicate.Code != "duplicate_document" || duplicate.Data.ID != other {
		t.Errorf("duplicate answer = %+v, want code duplicate_document and the other Document %s", duplicate, other)
	}

	// None of the rejected replacements changed the Document.
	resp := tr.get(t, "/documents/"+id, alice)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after the rejected replacements = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Data documentFileView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Data.FileName != "v1.txt" || out.Data.ParseStatus != "completed" {
		t.Errorf("Document = %+v, want v1 untouched by the rejected replacements", out.Data)
	}
}
