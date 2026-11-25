// Package router is Trelis's HTTP surface: the chi router and the server
// lifecycle around it.
package router

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// New returns the service's routes. GET /healthz is liveness and ignores
// dependencies; GET /readyz probes every dependency in ready. Both are open
// so orchestrators can probe them; everything else, unknown paths and
// methods included, needs an API key that authn accepts, so a caller
// without one cannot map the routes.
func New(ready Readiness, authn Authenticator) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Get("/healthz", healthz)
	r.Get("/readyz", ready.handler)

	authed := authenticate(authn)
	r.NotFound(authed(http.NotFoundHandler()).ServeHTTP)
	r.MethodNotAllowed(authed(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	})).ServeHTTP)

	r.Group(func(r chi.Router) {
		r.Use(authed)
		r.Get("/me", me)
	})
	return r
}

// healthz answers 200 for as long as the process is serving requests.
func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeError answers with {"error": msg}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
