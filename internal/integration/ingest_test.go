//go:build integration

package integration

import (
	"net/http"
	"testing"
)

func TestTrelisServesWhileTemporalIsUnreachable(t *testing.T) {
	tr := startWith(t, "TRELIS_TEMPORAL_HOST=127.0.0.1", "TRELIS_TEMPORAL_PORT=1")
	if code, checks := tr.readiness(t); code != http.StatusServiceUnavailable || checks["temporal"] == "up" || checks["postgres"] != "up" {
		t.Errorf("readyz = %d %v; want 503 with Temporal down and Postgres up", code, checks)
	}
}

func TestAnUnparseableSourceFileMarksTheDocumentFailed(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	// "not a zip" cannot be a docx in either build: the default build has no
	// engine at all, and the linked one rejects the bytes.
	id := tr.uploadBytes(t, alice, "broken.docx", []byte("not a zip"))

	doc := tr.waitForIngest(t, alice, id)
	if doc.ParseStatus != "failed" {
		t.Fatalf("parse_status = %q, want failed", doc.ParseStatus)
	}
	if doc.ParseError == "" {
		t.Error("parse_error is empty, want the reason the Source File could not be parsed")
	}
	if chunks := tr.chunkContents(t, id); len(chunks) != 0 {
		t.Errorf("Chunks = %q, want none for a failed parse", chunks)
	}
}
