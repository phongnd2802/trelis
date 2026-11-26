package handler

import (
	"net/http"

	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// PrincipalHandler answers requests about the calling Principal.
type PrincipalHandler struct {
	service interfaces.PrincipalService
}

// NewPrincipalHandler creates a new Principal handler.
func NewPrincipalHandler(service interfaces.PrincipalService) *PrincipalHandler {
	return &PrincipalHandler{service: service}
}

// Me answers GET /me with the calling Principal and their Groups, as the
// bare Principal rather than the response envelope (its shape predates the
// envelope and clients read it as is).
func (h *PrincipalHandler) Me(w http.ResponseWriter, r *http.Request) {
	p, err := h.service.GetCurrentPrincipal(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
