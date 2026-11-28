package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// SSEWriter frames Server-Sent Events on a ResponseWriter and flushes every
// frame, so a client receives each one before the handler returns. The caller
// sets Content-Type: text/event-stream and the other stream headers before
// the first write. The ingest progress and streamed answer streams use it.
type SSEWriter struct {
	w http.ResponseWriter
}

// NewSSEWriter returns a writer for w.
func NewSSEWriter(w http.ResponseWriter) *SSEWriter {
	return &SSEWriter{w: w}
}

// Event writes one event frame with its id, name and JSON data, then flushes.
// A JSON body is one line because encoding/json never inserts a newline, so
// the frame's data field stays on the line that starts it.
func (s *SSEWriter) Event(id int64, name string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", name, err)
	}
	if _, err := fmt.Fprintf(s.w, "id: %d\nevent: %s\ndata: %s\n\n", id, name, raw); err != nil {
		return fmt.Errorf("write %s event: %w", name, err)
	}
	return s.Flush()
}

// Comment writes an SSE comment frame and flushes. Clients ignore comments;
// the ingest stream sends one as a keepalive so a proxy does not close an
// idle stream.
func (s *SSEWriter) Comment(text string) error {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return fmt.Errorf("write stream comment: %w", err)
	}
	return s.Flush()
}

// Flush sends buffered bytes to the client. A ResponseWriter that cannot
// flush is an error: an SSE stream that buffers is not a stream. The response
// controller also finds Flush behind a middleware wrapper that implements
// Unwrap, as the access log's does.
func (s *SSEWriter) Flush() error {
	if err := http.NewResponseController(s.w).Flush(); err != nil {
		return fmt.Errorf("flush event stream: %w", err)
	}
	return nil
}
