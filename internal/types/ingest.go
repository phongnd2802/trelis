package types

// UnparseableError explains why a Document's Source File cannot be turned
// into text. Reason is short and carries no Document content or file name,
// because it is stored in the Document's parse_error and shown to its
// readers.
type UnparseableError struct {
	// Reason is what the Document's parse_error records.
	Reason string
	cause  error
}

// NewUnparseableError returns an UnparseableError with a reason safe to show
// and store, wrapping cause for server-side context.
func NewUnparseableError(reason string, cause error) *UnparseableError {
	return &UnparseableError{Reason: reason, cause: cause}
}

// Error returns the reason and, when there is one, the underlying cause.
func (e *UnparseableError) Error() string {
	if e.cause == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.cause.Error()
}

// Unwrap returns the error that caused the parse to fail.
func (e *UnparseableError) Unwrap() error { return e.cause }

// DocumentNotFoundError means the Document an ingest workflow was started for
// no longer exists. Retrying cannot help, and there is no row left to mark
// failed.
type DocumentNotFoundError struct{}

// Error returns the reason the workflow records in its history.
func (e *DocumentNotFoundError) Error() string { return "the Document does not exist" }

// EmbeddingUnconfiguredError means models.embedding names no provider, so a
// Document cannot be embedded. Retrying cannot help until an operator sets
// one, and the message is safe to store in the Document's parse_error.
type EmbeddingUnconfiguredError struct{}

// Error returns the reason shown beside a Document that failed ingest and
// recorded in the workflow history.
func (e *EmbeddingUnconfiguredError) Error() string { return "no embedding model is configured" }
