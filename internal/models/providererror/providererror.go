// Package providererror turns a model provider's rejection into the
// application error the HTTP API answers with. Every model role wraps its
// call's error here, so the mapping from provider status to client status is
// written once.
package providererror

import (
	"errors"
	"net/http"

	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/models/api"
)

// Wrap maps a provider's rejection, after the client's retries, to the
// client's error: an *apperrors.AppError with ErrModelProvider and 502 that
// names role and status, keeping the original *api.HTTPError in the chain so
// callers that classify by status, like ingest, still see it. A 401, 402, 403
// or 429 is a rejection of Trelis's credentials, credit, permission or rate,
// not of this request. Any other error is returned as it is.
func Wrap(role string, err error) error {
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || !rejected(httpErr.StatusCode) {
		return err
	}
	return apperrors.NewModelProviderError(role, httpErr.StatusCode, err)
}

// rejected reports whether status means the provider refused Trelis's call
// rather than failing to serve it.
func rejected(status int) bool {
	switch status {
	case http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusTooManyRequests:
		return true
	}
	return false
}
