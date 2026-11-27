package providererror

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/models/api"
)

func TestAProviderRejectionBecomesABadGatewayNamingTheRole(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusTooManyRequests,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			providerErr := &api.HTTPError{StatusCode: status, Body: `{"error":{"message":"secret vendor body"}}`}

			err := Wrap("rerank", providerErr)

			appErr, ok := apperrors.IsAppError(err)
			if !ok {
				t.Fatalf("Wrap = %v, want an AppError", err)
			}
			if appErr.Code != apperrors.ErrModelProvider || appErr.HTTPCode != http.StatusBadGateway {
				t.Errorf("code/status = %d/%d, want %d/502",
					appErr.Code, appErr.HTTPCode, apperrors.ErrModelProvider)
			}
			want := fmt.Sprintf("the rerank model provider rejected the call (HTTP %d)", status)
			if appErr.Message != want {
				t.Errorf("message = %q, want %q", appErr.Message, want)
			}
			if strings.Contains(appErr.Message, "secret vendor body") {
				t.Errorf("message carries the vendor body: %q", appErr.Message)
			}
			// Ingest classifies a failed batch by the provider status, so the
			// original HTTPError has to stay reachable through the AppError.
			if !errors.As(err, &providerErr) {
				t.Errorf("Wrap = %v, want the original *api.HTTPError in the chain", err)
			}
			if got := api.HTTPStatusCode(err); got != status {
				t.Errorf("api.HTTPStatusCode = %d, want %d", got, status)
			}
		})
	}
}

func TestAnotherProviderFailureIsLeftAlone(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			providerErr := &api.HTTPError{StatusCode: status}

			err := Wrap("embedding", providerErr)

			var httpErr *api.HTTPError
			if !errors.As(err, &httpErr) || httpErr != providerErr {
				t.Errorf("Wrap = %v, want the provider's own error back", err)
			}
			if _, ok := apperrors.IsAppError(err); ok {
				t.Errorf("Wrap = %v, want no AppError for a status that is not a rejection", err)
			}
		})
	}
}

func TestARejectionStaysClassifiableAfterIngestWrapsIt(t *testing.T) {
	providerErr := &api.HTTPError{StatusCode: http.StatusPaymentRequired}
	err := fmt.Errorf("embed 3 Chunks 0-3 of document d: %w",
		fmt.Errorf("embed texts 1-3: %w", Wrap("embedding", providerErr)))

	if got := api.HTTPStatusCode(err); got != http.StatusPaymentRequired {
		t.Errorf("api.HTTPStatusCode = %d, want %d", got, http.StatusPaymentRequired)
	}
	appErr, ok := apperrors.IsAppError(err)
	if !ok || appErr.Code != apperrors.ErrModelProvider {
		t.Errorf("IsAppError = %v, %v, want ErrModelProvider through the batch context", appErr, ok)
	}
}
