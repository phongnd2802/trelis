package modeltest

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// sse writes a text/event-stream response, flushing after every event so a
// client sees the stream arrive in pieces.
type sse struct {
	w http.ResponseWriter
}

// newSSE starts a 200 event stream on w.
func newSSE(w http.ResponseWriter) sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return sse{w: w}
}

// data writes one unnamed event carrying v as JSON.
func (s sse) data(v any) { s.event("", v) }

// event writes one event; name is omitted when empty.
func (s sse) event(name string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only our own response types are marshalled
	}
	if name != "" {
		_, _ = fmt.Fprintf(s.w, "event: %s\n", name)
	}
	s.raw(string(b))
}

// raw writes one data line as is.
func (s sse) raw(data string) {
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", data)
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

// pieces splits s into chunks of up to three words, keeping the
// whitespace, so concatenating them gives s back.
func pieces(s string) []string {
	const wordsPerPiece = 3
	var out []string
	start, words := 0, 0
	for i := 1; i < len(s); i++ {
		if s[i] != ' ' && s[i-1] == ' ' {
			if words++; words == wordsPerPiece {
				out = append(out, s[start:i])
				start, words = i, 0
			}
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
