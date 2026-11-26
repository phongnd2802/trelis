package ingest

import (
	"testing"

	"go.temporal.io/sdk/temporal"
)

func TestFailureReasonOnlyShowsAnUnparseableMessage(t *testing.T) {
	// A retried storage or database failure arrives as an application error
	// whose message is the raw Go error, which can name an endpoint, bucket
	// or key. It must not become the Document's parse_error.
	internal := temporal.NewApplicationError(
		`put object: https://silo.internal:9000/private-bucket/documents/1/source: access denied`, "Error")
	if got := failureReason(internal); got != "the Document could not be ingested" {
		t.Errorf("failureReason(internal error) = %q, want the generic sentence", got)
	}

	unparseable := temporal.NewNonRetryableApplicationError(
		"the Source File could not be parsed", unparseableDocumentType, nil)
	if got := failureReason(unparseable); got != "the Source File could not be parsed" {
		t.Errorf("failureReason(unparseable) = %q, want the parser's safe reason", got)
	}

	// The same type with no message falls back rather than storing "".
	empty := temporal.NewNonRetryableApplicationError("", unparseableDocumentType, nil)
	if got := failureReason(empty); got != "the Document could not be ingested" {
		t.Errorf("failureReason(empty message) = %q, want the generic sentence", got)
	}
}
