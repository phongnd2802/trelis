package router

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/phongnd2802/trelis/internal/auth"
)

// Authenticator resolves an API key to the Principal it belongs to. It
// returns auth.ErrUnauthenticated for a key that belongs to no one; any other
// error means the key could not be checked.
type Authenticator interface {
	Authenticate(ctx context.Context, key string) (auth.Principal, error)
}

// authenticate rejects a request with 401 unless it carries
// "Authorization: Bearer <key>" for a known key, and puts the Principal in
// the request context for the handlers behind it.
func authenticate(authn Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := bearer(r.Header.Get("Authorization"))
			if !ok {
				unauthorized(w, "missing API key: send Authorization: Bearer <key>")
				return
			}
			p, err := authn.Authenticate(r.Context(), key)
			if errors.Is(err, auth.ErrUnauthenticated) {
				unauthorized(w, "invalid API key")
				return
			}
			if err != nil {
				slog.ErrorContext(r.Context(), "authenticate", "err", err)
				writeError(w, http.StatusInternalServerError, "could not check API key")
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), p)))
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

// me answers GET /me with the calling Principal and their Groups.
func me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	writeJSON(w, http.StatusOK, p)
}
