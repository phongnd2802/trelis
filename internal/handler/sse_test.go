package handler

import (
	"bytes"
	"net/http"
	"testing"
)

// flushWriter is a ResponseWriter that records its body and counts flushes.
type flushWriter struct {
	header  http.Header
	body    bytes.Buffer
	flushes int
}

func newFlushWriter() *flushWriter { return &flushWriter{header: http.Header{}} }

func (w *flushWriter) Header() http.Header { return w.header }

func (w *flushWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *flushWriter) WriteHeader(int) {}

func (w *flushWriter) Flush() { w.flushes++ }

// unwrapping hides the base writer behind Unwrap, the way a middleware
// wrapper that does not implement Flush would.
type unwrapping struct{ http.ResponseWriter }

func (w unwrapping) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestSSEWriterFramesAndFlushesAnEvent(t *testing.T) {
	base := newFlushWriter()
	if err := NewSSEWriter(base).Event(7, "completed", struct {
		Chunks int `json:"chunks"`
	}{3}); err != nil {
		t.Fatalf("Event: %v", err)
	}

	want := "id: 7\nevent: completed\ndata: {\"chunks\":3}\n\n"
	if got := base.body.String(); got != want {
		t.Errorf("frame = %q, want %q", got, want)
	}
	if base.flushes != 1 {
		t.Errorf("flushes = %d, want one per frame", base.flushes)
	}
}

func TestSSEWriterWritesAKeepaliveComment(t *testing.T) {
	base := newFlushWriter()
	if err := NewSSEWriter(base).Comment("keepalive"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if got := base.body.String(); got != ": keepalive\n\n" {
		t.Errorf("comment = %q, want : keepalive with a blank line", got)
	}
	if base.flushes != 1 {
		t.Errorf("flushes = %d, want one", base.flushes)
	}
}

func TestSSEWriterFindsFlushThroughUnwrap(t *testing.T) {
	base := newFlushWriter()
	if err := NewSSEWriter(unwrapping{base}).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if base.flushes != 1 {
		t.Errorf("flushes = %d, want the writer behind Unwrap to flush", base.flushes)
	}
}

// A ResponseWriter with no flush at all is an error the handler can report
// instead of pretending the stream is live.
type noFlushWriter struct {
	header http.Header
	body   bytes.Buffer
}

func (w *noFlushWriter) Header() http.Header         { return w.header }
func (w *noFlushWriter) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *noFlushWriter) WriteHeader(int)             {}

func TestSSEWriterReportsAWriterThatCannotFlush(t *testing.T) {
	w := &noFlushWriter{header: http.Header{}}
	if err := NewSSEWriter(w).Flush(); err == nil {
		t.Fatal("Flush = nil on a writer with no Flush, want ErrNotSupported")
	}
}
