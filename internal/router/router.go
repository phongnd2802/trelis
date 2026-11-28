// Package router is Trelis's HTTP surface: the chi router and the server
// lifecycle around it.
package router

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	_ "github.com/phongnd2802/trelis/internal/apidocs"
	"github.com/phongnd2802/trelis/internal/handler"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// New returns the service's routes. GET /healthz is liveness and ignores
// dependencies; GET /readyz probes every dependency in ready. Both are open
// so orchestrators can probe them, as are /swagger/* (the UI and its spec)
// when swagger is true. Everything else, unknown paths and methods included,
// needs an API key that authn accepts, so a caller without one cannot map
// the routes. principals serves /me, docs /documents, retrieval /retrieve,
// answers /answer and chunks /chunks; any may be nil only where its routes
// are never called.
//
//	@title						Trelis API
//	@version					0.1.0
//	@description				HTTP API for uploading, reading, searching and questioning Documents. Every route except the probes needs an API key.
//	@BasePath					/
//
//	@securityDefinitions.apikey	bearerAuth
//	@in							header
//	@name						Authorization
//	@description				Paste "Bearer <key>", with a key from `trelis admin key create <principal>` (`go run ./cmd/server admin key create <principal>` in dev).
func New(ready Readiness, authn interfaces.AuthService,
	principals *handler.PrincipalHandler, docs *handler.DocumentHandler,
	retrieval *handler.RetrievalHandler, answers *handler.AnswerHandler,
	chunks *handler.ChunkHandler, swagger bool,
) http.Handler {
	r := chi.NewRouter()
	// Outermost first: every response gets an ID and a log line, and a panic
	// still reaches the access log as a 500.
	r.Use(requestID)
	r.Use(accessLog)
	r.Use(recoverer)
	if swagger {
		r.Get("/swagger/*", httpSwagger.Handler(httpSwagger.PersistAuthorization(true)))
	}
	r.Get("/healthz", healthz)
	r.Get("/readyz", ready.handler)

	authed := authenticate(authn)
	r.NotFound(authed(http.NotFoundHandler()).ServeHTTP)
	r.MethodNotAllowed(authed(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	})).ServeHTTP)

	r.Group(func(r chi.Router) {
		r.Use(authed)
		r.Get("/me", principals.Me)
		r.Post("/documents", docs.CreateDocumentFromFile)
		r.Get("/documents", docs.ListDocuments)
		r.Get("/documents/{id}", docs.GetDocument)
		r.Delete("/documents/{id}", docs.DeleteDocument)
		r.Get("/documents/{id}/file", docs.DownloadDocumentFile)
		r.Post("/retrieve", retrieval.Retrieve)
		r.Post("/answer", answers.Answer)
		r.Get("/chunks/{id}", chunks.GetChunk)
	})
	return r
}

// HealthStatus is what GET /healthz answers with.
type HealthStatus struct {
	Status string `json:"status"`
}

// ErrorResponse is what the router answers an error with itself, as
// {"error": msg}: the auth middleware's 401, an unknown path's 404 and an
// unknown method's 405.
type ErrorResponse struct {
	Error string `json:"error"`
}

// healthz answers 200 for as long as the process is serving requests.
//
//	@Summary		Liveness probe
//	@Description	Answers 200 while the process is serving requests, whatever its dependencies are.
//	@Tags			system
//	@Produce		json
//	@Success		200	{object}	HealthStatus
//	@Router			/healthz [get]
func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthStatus{Status: "ok"})
}

// writeError answers with {"error": msg}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
