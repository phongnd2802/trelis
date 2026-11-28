package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWantsEventStreamHonoursQualityValues(t *testing.T) {
	for accept, want := range map[string]bool{
		"":                      false,
		"application/json":      false,
		"*/*":                   false,
		"text/event-stream":     true,
		"text/event-stream;q=0": false,
		"application/json;q=1, text/event-stream;q=0.1":   false,
		"application/json;q=0.5, text/event-stream;q=0.5": true,
		"text/event-stream;q=0.5, */*;q=1":                false,
		"text/event-stream;q=1, */*;q=0.5":                true,
		"text/event-stream;q=foo":                         false,
		"text/event-stream; charset=utf-8":                true,
	} {
		req := httptest.NewRequest(http.MethodPost, "/answer", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if got := wantsEventStream(req); got != want {
			t.Errorf("wantsEventStream(%q) = %v, want %v", accept, got, want)
		}
	}
}
