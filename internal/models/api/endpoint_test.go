package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The body can echo request content, so it must not appear in the message
// that reaches logs; callers that need it read Body.
func TestHTTPErrorDoesNotPutTheBodyInItsMessage(t *testing.T) {
	err := &HTTPError{StatusCode: http.StatusBadRequest, Body: `{"error":"secret prompt"}`}

	if got := err.Error(); strings.Contains(got, "secret prompt") || got != "API request failed with status 400" {
		t.Errorf("Error() = %q, want only the status", got)
	}
	if !strings.Contains(err.Body, "secret prompt") {
		t.Errorf("Body = %q, want it kept for inspection", err.Body)
	}
}

func TestHTTPStatusCode(t *testing.T) {
	if got := HTTPStatusCode(&HTTPError{StatusCode: http.StatusTooManyRequests}); got != http.StatusTooManyRequests {
		t.Errorf("HTTPStatusCode(HTTPError) = %d, want 429", got)
	}
	if got := HTTPStatusCode(errors.New("other")); got != 0 {
		t.Errorf("HTTPStatusCode(other) = %d, want 0", got)
	}
	if got := HTTPStatusCode(nil); got != 0 {
		t.Errorf("HTTPStatusCode(nil) = %d, want 0", got)
	}
}
