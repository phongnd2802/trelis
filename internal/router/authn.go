package router

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// authenticate rejects a request with 401 unless it carries
// "Authorization: Bearer <key>" for a known key, and puts the Principal in
// the request context for the handlers behind it.
func authenticate(authn interfaces.AuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := bearer(r.Header.Get("Authorization"))
			if !ok {
				unauthorized(w, "missing API key: send Authorization: Bearer <key>")
				return
			}
			p, err := authn.Authenticate(r.Context(), key)
			if errors.Is(err, interfaces.ErrUnauthenticated) {
				unauthorized(w, "invalid API key")
				return
			}
			if err != nil {
				slog.ErrorContext(r.Context(), "authenticate", "err", err)
				writeError(w, http.StatusInternalServerError, "could not check API key")
				return
			}
			next.ServeHTTP(w, r.WithContext(types.WithPrincipal(r.Context(), p)))
		})
	}
}

// bearer extracts the token from an Authorization header value.
func bearer(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// unauthorized answers 401 with the challenge RFC 6750 asks for.
func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, msg)
}
